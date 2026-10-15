package direct

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/host"
)

// Result codes. Only CodeUnavailable means "this session has no live direct
// channel"; callers may then use the session's UI channel instead. Every other
// failure is final for this attempt.
const (
	CodeReady       = "direct_ready"
	CodeUnavailable = "direct_unavailable"
	CodeBusy        = "session_busy"
	CodeAmbiguous   = "direct_target_ambiguous"
	CodeDelivered   = "direct_delivered"
	CodeUnconfirmed = "direct_sent_unconfirmed"
	CodeUncertain   = "submitted_uncertain"
	CodeInvalid     = "message_invalid"
)

// supportedProtocol is the registry peerProtocol whose wire format this client
// speaks: newline-delimited JSON, an optional auth frame (required only on
// Windows), then {"type":"user",...} frames.
const supportedProtocol = 1

// maxLine mirrors the receiver's per-line limit.
const maxLine = 1 << 20

// Sender delivers one message per call into a live Claude Code session through
// the session's own messaging socket. The frame names the expected CLI session
// id, which the receiver checks against itself, so a reused PID or socket path
// can never route the text to another conversation.
type Sender struct {
	dirs    func() []string
	lookup  func(int) (host.Process, bool)
	dial    func(ctx context.Context, path string) (net.Conn, error)
	sleep   func(context.Context, time.Duration) error
	uid     int
	confirm time.Duration // how long to watch the registry for the session turning busy
}

func NewSender() *Sender {
	return &Sender{
		dirs:   SessionsDirs,
		lookup: host.Lookup,
		dial: func(ctx context.Context, path string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, "unix", path)
		},
		sleep:   sleepContext,
		uid:     os.Getuid(),
		confirm: 8 * time.Second,
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func result(ok, submitted bool, code, message string) domain.BridgeResult {
	// AccessibilityGranted follows the terminal convention: true means this
	// channel has no Accessibility prerequisite, not that permission was probed.
	return domain.BridgeResult{OK: ok, Submitted: submitted, Code: code, Message: message, AccessibilityGranted: true, Channel: domain.ChannelDirect}
}

// Unavailable reports whether r means the session simply has no live direct
// channel, so nothing was attempted through it.
func Unavailable(r domain.BridgeResult) bool {
	return r.Channel == domain.ChannelDirect && r.Code == CodeUnavailable && !r.Submitted
}

// expectedHost is the desktop owner the binding was planned for, or "" for a
// terminal session.
func expectedHost(b domain.Binding) string {
	if b.Origin != nil && b.Origin.HostSessionID != "" {
		return b.Origin.HostSessionID
	}
	if id, ok := strings.CutPrefix(b.DeepLink, "claude://claude.ai/epitaxy/"); ok && domain.ValidHostSessionID(id) {
		return id
	}
	return ""
}

// locate finds the single live registry record of the binding's session.
func (s *Sender) locate(b domain.Binding) (Entry, *domain.BridgeResult) {
	unavailable := func(msg string) (Entry, *domain.BridgeResult) {
		r := result(false, false, CodeUnavailable, msg)
		return Entry{}, &r
	}
	if !domain.ValidCLISessionID(b.SessionID) {
		return unavailable("会话 ID 不是 Claude Code 的 UUID，无法使用后台直发。")
	}
	desktop := b.Executor == "" || b.Executor == domain.ExecutorDesktop
	wantHost := expectedHost(b)
	if desktop && wantHost == "" {
		return unavailable("缺少桌面会话 ID，无法核验后台直发目标。")
	}
	var live []Entry
	seen := map[int]bool{}
	for _, dir := range s.dirs() {
		entries, err := readRegistry(dir, s.uid)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.SessionID != b.SessionID || seen[e.PID] || !s.alive(e) {
				continue
			}
			seen[e.PID] = true
			live = append(live, e)
		}
	}
	switch {
	case len(live) == 0:
		return unavailable("会话当前没有运行中的 Claude Code 进程，后台直发不可用。")
	case len(live) > 1:
		r := result(false, false, CodeAmbiguous, "同一会话有多个运行中的进程，无法确定发送目标，已停止。")
		return Entry{}, &r
	}
	e := live[0]
	if (desktop && e.HostSessionID != wantHost) || (!desktop && e.HostSessionID != "") {
		r := result(false, false, CodeAmbiguous, "运行中的进程属于另一个宿主会话，已停止发送。")
		return Entry{}, &r
	}
	if e.PeerProtocol != supportedProtocol {
		return unavailable(fmt.Sprintf("会话的消息协议版本为 %d，LimitBreak 只支持版本 %d，改用界面发送。", e.PeerProtocol, supportedProtocol))
	}
	if !s.trustedSocket(e.SocketPath) {
		return unavailable("会话的消息端口不存在或不属于当前用户，改用界面发送。")
	}
	return e, nil
}

// alive accepts a record only while its PID still runs the process that wrote
// it: the kernel start time must match the recorded procStart (second
// precision), so a reused PID is never trusted.
func (s *Sender) alive(e Entry) bool {
	if e.PID <= 1 {
		return false
	}
	recorded, ok := e.ParseProcStart()
	if !ok {
		return false
	}
	p, ok := s.lookup(e.PID)
	if !ok || p.Start.IsZero() {
		return false
	}
	d := p.Start.Sub(recorded)
	return d > -2*time.Second && d < 2*time.Second
}

// trustedSocket requires a clean absolute path to a socket owned by the user,
// inaccessible to others, inside a directory only the user can write to.
func (s *Sender) trustedSocket(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 103 {
		return false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != s.uid || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	dir, err := os.Lstat(filepath.Dir(path))
	return err == nil && dir.IsDir() && ownedBy(dir, s.uid)
}

// Preflight reports whether a direct send could happen now without writing
// anything. A busy session is retried later rather than interrupted.
func (s *Sender) Preflight(_ context.Context, b domain.Binding) domain.BridgeResult {
	e, problem := s.locate(b)
	if problem != nil {
		return *problem
	}
	if e.Status != "idle" {
		return result(false, false, CodeBusy, "会话正在处理上一轮，空闲后再后台直发。")
	}
	return result(true, false, CodeReady, fmt.Sprintf("会话进程 %d 在线且空闲，将通过后台直发送达，无需切换窗口。", e.PID))
}

type userFrame struct {
	Type      string      `json:"type"`
	Message   userMessage `json:"message"`
	SessionID string      `json:"session_id"`
	UUID      string      `json:"uuid"`
	Priority  string      `json:"priority"`
}

type userMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Frame returns the exact line written for message to sessionID.
func Frame(sessionID, message, uuid string) ([]byte, error) {
	line, err := json.Marshal(userFrame{Type: "user", Message: userMessage{Role: "user", Content: message}, SessionID: sessionID, UUID: uuid, Priority: "next"})
	if err != nil {
		return nil, err
	}
	line = append(line, '\n')
	if len(line) > maxLine {
		return nil, errors.New("frame too large")
	}
	return line, nil
}

// Resume writes one user frame. Nothing is retried after the first byte: a
// failure once the line may have reached the receiver is reported uncertain.
func (s *Sender) Resume(ctx context.Context, b domain.Binding, message string) (domain.BridgeResult, error) {
	if !domain.ValidContinueMessage(message) {
		return result(false, false, CodeInvalid, "继续文案无效。"), nil
	}
	e, problem := s.locate(b)
	if problem != nil {
		return *problem, nil
	}
	if e.Status != "idle" {
		return result(false, false, CodeBusy, "会话正在处理上一轮，空闲后再后台直发。"), nil
	}
	id, err := newUUID()
	if err != nil {
		return result(false, false, CodeUnavailable, "无法生成消息 ID，改用界面发送。"), nil
	}
	line, err := Frame(e.SessionID, message, id)
	if err != nil {
		return result(false, false, CodeInvalid, "继续文案过长。"), nil
	}
	conn, err := s.dial(ctx, e.SocketPath)
	if err != nil {
		if ctx.Err() != nil {
			return result(false, false, CodeUnavailable, "发送已取消，未写入任何内容。"), ctx.Err()
		}
		// Nothing was written; the receiver is gone or not listening yet.
		return result(false, false, CodeUnavailable, "会话的消息端口未接受连接，改用界面发送。"), nil
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	n, err := conn.Write(line)
	if err != nil || n != len(line) {
		if n == 0 {
			return result(false, false, CodeUnavailable, "会话的消息端口拒绝写入，未发送任何内容，改用界面发送。"), nil
		}
		// The receiver only acts on a complete line, but a partial write cannot
		// prove it saw none of it; never resend.
		return result(false, false, CodeUncertain, "消息只写入了一部分，结果不确定，不会自动重发。"), nil
	}
	// Half-close and wait briefly for the receiver to finish reading the line.
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var sink [512]byte
	for {
		if _, err := conn.Read(sink[:]); err != nil {
			break
		}
	}
	if s.watchBusy(ctx, e) {
		return result(true, true, CodeDelivered, "已后台直发，会话已开始处理；等待会话回执确认。"), nil
	}
	return result(true, true, CodeUnconfirmed,
		"已后台直发，但会话尚未开始处理。若该会话处于「跳过权限确认」模式，Claude 会先把这条消息放进待批准列表，需要你在会话里批准；结果以会话回执为准，不会自动重发。"), nil
}

// watchBusy polls the session's registry record until it reports a turn in
// progress, which is the only delivery signal a non-session client can see.
func (s *Sender) watchBusy(ctx context.Context, sent Entry) bool {
	deadline := time.Now().Add(s.confirm)
	for {
		for _, dir := range s.dirs() {
			entries, err := readRegistry(dir, s.uid)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.PID == sent.PID && e.SessionID == sent.SessionID && e.ProcStart == sent.ProcStart &&
					(e.Status == "busy" || e.StatusUpdated > sent.StatusUpdated) {
					return true
				}
			}
		}
		if !time.Now().Before(deadline) || s.sleep(ctx, 250*time.Millisecond) != nil {
			return false
		}
	}
}
