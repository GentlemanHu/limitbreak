package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"quota-session-orchestrator/internal/config"
	"quota-session-orchestrator/internal/domain"
)

func normalizeHook(raw []byte, observed time.Time) (domain.HookEvent, error) {
	var e domain.HookEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, err
	}
	var metadata struct {
		ToModel        string `json:"to_model"`
		TranscriptPath string `json:"transcript_path"`
	}
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return e, err
	}
	if metadata.ToModel != "" {
		e.NewModel = metadata.ToModel
	}
	if e.SessionID == "" || strings.ContainsAny(e.SessionID, "/\x00\r\n") || len(e.SessionID) > 256 {
		return e, errors.New("Hook 会话标识无效")
	}
	switch e.HookEventName {
	case "Notification":
		switch e.NotificationType {
		case "quota_auto_resume_fired", "quota_auto_resume_stale", "quota_auto_resume_disabled":
		default:
			return e, errors.New("Hook 配额通知不支持")
		}
	case "SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd", "PostModelSwitch":
	default:
		return e, errors.New("Hook 事件不支持")
	}
	e.ObservedAt = observed.UTC()
	e.Kind = "code" // This executable is installed as a Claude Code hook.
	e.Origin = nil  // Origin comes from the process environment, never from hook input.
	if !domain.ValidPermissionMode(e.PermissionMode) {
		e.PermissionMode = ""
	}
	if e.EventID == "" {
		// Transcript metadata distinguishes separate turns without reading content.
		if metadata.TranscriptPath != "" {
			if st, err := os.Stat(metadata.TranscriptPath); err == nil && st.Mode().IsRegular() {
				h := sha256.New()
				_, _ = h.Write(raw)
				_, _ = fmt.Fprintf(h, "%d:%d", st.Size(), st.ModTime().UnixNano())
				e.EventID = hex.EncodeToString(h.Sum(nil))
			}
		}
		if e.EventID == "" {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return e, err
			}
			e.EventID = hex.EncodeToString(b)
		}
	}
	// No prompt, last_assistant_message or full transcript is kept in the envelope.
	if len(e.ErrorDetails) > 2000 {
		e.ErrorDetails = e.ErrorDetails[:2000]
	}
	if e.SessionTitle == "" && metadata.TranscriptPath != "" {
		// Hook input carries no title; the sidebar name lives in the transcript.
		e.SessionTitle = transcriptTitle(metadata.TranscriptPath, e.SessionID)
	}
	if len(e.SessionTitle) > 500 {
		e.SessionTitle = e.SessionTitle[:500]
	}
	return e, nil
}

func forwardHook(path string) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("Hook 输入超过 1 MiB")
	}
	now := time.Now()
	e, err := normalizeHook(raw, now)
	if err != nil {
		return err
	}
	e.Origin = captureOrigin(os.Getenv, os.Getppid(), parentPID, executablePath, now)
	key := os.Getenv(c.ManagementKeyEnv)
	if key != "" {
		e.ErrorDetails = strings.ReplaceAll(e.ErrorDetails, key, "[REDACTED]")
	}
	e.ErrorDetails = strings.ReplaceAll(e.ErrorDetails, c.Token, "[REDACTED]")
	return spoolAndDeliver(path, c, e)
}

// spoolAndDeliver writes the event durably first, then delivers it. Retrying
// the same envelope cannot create a second job; an unreachable daemon drains
// the spool on its next start.
func spoolAndDeliver(path string, c config.Config, e domain.HookEvent) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	spool := filepath.Join(filepath.Dir(path), "spool")
	if err := os.MkdirAll(spool, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(spool)
	if err != nil {
		return err
	}
	if len(entries) >= 10000 {
		return errors.New("Hook 离线队列已满，请检查守护器")
	}
	// Write first, then deliver. Retrying the same envelope cannot create a second job.
	name := filepath.Join(spool, hexHash(e.EventID)+".json")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	if _, err := call(ctx, c, "POST", "/api/hooks", b); err == nil {
		if err := os.Remove(name); err != nil {
			return err
		}
	}
	// An unreachable daemon leaves a durable event, not a blocked user turn.
	return nil
}

func hexHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

type eventEngine interface {
	HandleEvent(context.Context, domain.HookEvent) error
}

func drainSpool(ctx context.Context, path string, e eventEngine) {
	dir := filepath.Join(filepath.Dir(path), "spool")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		entries, _ := os.ReadDir(dir)
		for _, entry := range entries {
			if ctx.Err() != nil {
				return
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			name := filepath.Join(dir, entry.Name())
			st, err := os.Lstat(name)
			if err != nil || !st.Mode().IsRegular() || st.Size() > 256<<10 {
				continue
			}
			b, err := os.ReadFile(name)
			if err != nil {
				continue
			}
			var event domain.HookEvent
			if json.Unmarshal(b, &event) != nil || event.SessionID == "" || event.EventID == "" {
				continue
			}
			if err := e.HandleEvent(ctx, event); err == nil {
				_ = os.Remove(name)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func hookSettings(path, version string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if _, err := config.Load(abs); err != nil {
		return err
	}
	if !versionAtLeast(version, 2, 1, 78) {
		return errors.New("请使用 --engine-version 提供已确认的内嵌引擎版本；StopFailure 至少需要 2.1.78")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	command := shellQuote(executable) + " hook --config " + shellQuote(abs)
	events := []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "SessionEnd"}
	if versionAtLeast(version, 2, 1, 251) {
		events = append(events, "PostModelSwitch")
	}
	hooks := map[string]any{}
	for _, event := range events {
		hooks[event] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": command, "timeout": 2}}}}
	}
	if versionAtLeast(version, 2, 1, 234) {
		hooks["Notification"] = []any{map[string]any{
			"matcher": "quota_auto_resume_fired|quota_auto_resume_stale|quota_auto_resume_disabled",
			"hooks":   []any{map[string]any{"type": "command", "command": command, "timeout": 2}},
		}}
	}
	data, err := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func versionAtLeast(version string, major, minor, patch int) bool {
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) < 3 {
		return false
	}
	want := []int{major, minor, patch}
	for i := 0; i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return false
		}
		if n > want[i] {
			return true
		}
		if n < want[i] {
			return false
		}
	}
	return true
}

func call(ctx context.Context, c config.Config, method, path string, body []byte) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, method, "http://"+c.Listen+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(r)
	if err != nil {
		return nil, errors.New("无法连接本地守护器")
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("本地接口 HTTP %d", res.StatusCode)
	}
	return data, nil
}
