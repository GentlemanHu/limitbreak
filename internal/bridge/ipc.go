package bridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"quota-session-orchestrator/internal/domain"
)

// AppIPC never falls back to a separately authorized executable.
// The signed App is both the permission requester and the AX executor.
type AppIPC struct {
	socket    string
	tokenFile string
	pathErr   error
}

type appRequest struct {
	Protocol        int             `json:"protocol"`
	RequestID       string          `json:"request_id"`
	Token           string          `json:"token"`
	Operation       string          `json:"operation"`
	Generation      int64           `json:"generation,omitempty"`
	LeaseID         string          `json:"lease_id,omitempty"`
	MessageHash     string          `json:"message_hash,omitempty"`
	Binding         *domain.Binding `json:"binding,omitempty"`
	Message         string          `json:"message,omitempty"`
	UserIdleSeconds int             `json:"user_idle_seconds,omitempty"`
	QuietSeconds    int             `json:"quiet_seconds,omitempty"`
	RestoreFocus    bool            `json:"restore_focus,omitempty"`
	Deadline        string          `json:"deadline"`
}

type appResponse struct {
	Protocol   int             `json:"protocol"`
	RequestID  string          `json:"request_id"`
	Result     json.RawMessage `json:"result"`
	Inspection json.RawMessage `json:"inspection,omitempty"`
}

func NewAppIPC(socket, tokenFile string) *AppIPC {
	c := &AppIPC{socket: socket, tokenFile: tokenFile}
	if !filepath.IsAbs(socket) || !filepath.IsAbs(tokenFile) || filepath.Clean(socket) != socket || filepath.Clean(tokenFile) != tokenFile || strings.ContainsRune(socket+tokenFile, '\x00') || len(socket) > 100 || filepath.Dir(socket) != filepath.Dir(tokenFile) {
		c.pathErr = errors.New("desktop IPC paths must share a private absolute directory")
	}
	return c
}

func (c *AppIPC) Probe(ctx context.Context) domain.BridgeResult {
	result, _, _ := c.invoke(ctx, appRequest{Operation: "probe"}, probeTimeout)
	return result
}

func (c *AppIPC) ValidateTarget(ctx context.Context, binding domain.Binding) domain.BridgeResult {
	if err := domain.ValidateResumeFields(binding); err != nil {
		return closed("invalid_binding", "桌面目标绑定格式无效。")
	}
	result, _, _ := c.invoke(ctx, appRequest{Operation: "validate_target", Binding: wire(binding)}, probeTimeout)
	return result
}

func (c *AppIPC) Inspect(ctx context.Context) (json.RawMessage, error) {
	result, inspection, err := c.invoke(ctx, appRequest{Operation: "inspect"}, probeTimeout)
	if err != nil {
		return nil, err
	}
	if !result.OK || len(inspection) == 0 {
		return nil, errors.New("desktop structure inspection is unavailable")
	}
	return inspection, nil
}

// wire strips daemon-only facts; the App receives only what it must verify.
func wire(b domain.Binding) *domain.Binding {
	b.MonitorResources, b.ResumeResources, b.MonitorMode = nil, nil, ""
	b.Origin, b.TargetOverride, b.Inferred = nil, "", false
	return &b
}

// deepLinkTarget is a hosted desktop session addressed by its own link.
func deepLinkTarget(b domain.Binding) bool {
	return b.Kind == "code" && b.Executor == domain.ExecutorDesktop && domain.ValidDesktopDeepLink(b.DeepLink)
}

func legacyGuardTarget(b domain.Binding) bool {
	return b.Kind == "code" && b.AutoResume && b.AcknowledgeSideEffects && b.ResumeOwner == "guard" && b.NativeAutoResumeDisabledAcknowledged &&
		b.ProjectMarker != "" && (b.TargetIdentifier != "" || b.TargetTitle != "")
}

func (c *AppIPC) Resume(ctx context.Context, binding domain.Binding, message string) (domain.BridgeResult, error) {
	if err := domain.ValidateResumeFields(binding); err != nil {
		return closed("invalid_binding", "绑定或恢复所有权无效。"), err
	}
	// The App re-checks generation, lease and message hash with the daemon before writing.
	if !deepLinkTarget(binding) && !legacyGuardTarget(binding) {
		return closed("consent_required", "仅允许可定位的托管 Code 会话或具有守护器所有权的绑定。"), nil
	}
	if binding.SessionID == "" || binding.ExecutionGeneration <= 0 || binding.ExecutionLease == "" {
		return closed("invalid_binding", "缺少会话、代次或执行租约。"), nil
	}
	if !utf8.ValidString(message) || strings.TrimSpace(message) == "" || len(message) > 4096 || strings.ContainsRune(message, '\x00') {
		return closed("invalid_message", "继续文案无效。"), nil
	}
	request := leased(binding, "resume", message)
	request.Message = message
	result, _, err := c.invoke(ctx, request, resumeTimeout)
	return result, err
}

// leased carries the execution lease, the hash of the message the lease is for,
// and the user-attention settings the App enforces.
func leased(binding domain.Binding, operation, message string) appRequest {
	digest := sha256.Sum256([]byte(message))
	request := appRequest{Operation: operation, Generation: binding.ExecutionGeneration, LeaseID: binding.ExecutionLease, MessageHash: hex.EncodeToString(digest[:]),
		Binding: wire(binding), UserIdleSeconds: binding.ExecutionUserIdleSeconds, QuietSeconds: binding.ExecutionQuietSeconds, RestoreFocus: binding.ExecutionRestoreFocus}
	if request.UserIdleSeconds == 0 {
		request.UserIdleSeconds = 3
	}
	return request
}

// Wake opens a hosted session's own link in the background so Claude Desktop
// starts the session's Claude Code process and its silent message socket. It
// types nothing and needs no Accessibility; the App re-checks the same lease,
// user idle time and Claude quiet time as a send, and restores focus.
func (c *AppIPC) Wake(ctx context.Context, binding domain.Binding, message string) domain.BridgeResult {
	if !deepLinkTarget(binding) {
		return closed("wake_unavailable", "该会话没有可用的桌面深链接。")
	}
	if binding.SessionID == "" || binding.ExecutionGeneration <= 0 || binding.ExecutionLease == "" {
		return closed("invalid_binding", "缺少会话、代次或执行租约。")
	}
	if !utf8.ValidString(message) || strings.TrimSpace(message) == "" || len(message) > 4096 || strings.ContainsRune(message, '\x00') {
		return closed("invalid_message", "继续文案无效。")
	}
	result, _, _ := c.invoke(ctx, leased(binding, "wake", message), wakeTimeout)
	return result
}

// Locate opens the session's deep link in Claude without typing anything.
func (c *AppIPC) Locate(ctx context.Context, binding domain.Binding) domain.BridgeResult {
	if !deepLinkTarget(binding) {
		return closed("locate_unavailable", "该会话没有可用的桌面深链接。")
	}
	result, _, _ := c.invoke(ctx, appRequest{Operation: "locate", Binding: wire(binding)}, probeTimeout)
	return result
}

func (c *AppIPC) invoke(ctx context.Context, request appRequest, timeout time.Duration) (domain.BridgeResult, json.RawMessage, error) {
	if ctx == nil || c == nil || c.pathErr != nil {
		return closed("bridge_unavailable", "原生应用 IPC 配置无效。"), nil, errors.New("invalid desktop IPC configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case processGate <- struct{}{}:
		defer func() { <-processGate }()
	case <-ctx.Done():
		return closed("cancelled", "桌面操作在提交请求前已取消。"), nil, ctx.Err()
	}
	token, err := privateIPCToken(c.socket, c.tokenFile)
	if err != nil {
		return closed("bridge_unavailable", "原生应用未运行或 IPC 私有身份不可核验。"), nil, errors.New("desktop IPC identity is unavailable")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return closed("bridge_unavailable", "无法生成请求身份。"), nil, err
	}
	request.Protocol = 1
	request.RequestID = hex.EncodeToString(nonce[:])
	request.Token = token
	deadline, _ := ctx.Deadline()
	request.Deadline = deadline.UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(request)
	if err != nil || len(payload)+1 > maxRequest {
		return closed("invalid_request", "桌面请求超过安全上限。"), nil, errors.New("invalid desktop IPC request")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", c.socket)
	if err != nil {
		return closed("bridge_unavailable", "无法连接原生应用执行器。"), nil, errors.New("desktop IPC connection failed")
	}
	defer connection.Close()
	if err := connection.SetDeadline(deadline); err != nil {
		return closed("bridge_unavailable", "无法设置执行时限。"), nil, err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stop:
		}
	}()
	sent := false
	uncertain := func(cause error) (domain.BridgeResult, json.RawMessage, error) {
		result := closed("bridge_failed", "原生应用未返回可靠执行结果。")
		if request.Operation == "resume" && sent {
			result.Submitted = true
			result.Code = "submitted_uncertain"
			result.Message = "恢复请求已交给原生应用，但结果未确认；禁止自动重发。"
		}
		return result, nil, cause
	}
	n, err := connection.Write(append(payload, '\n'))
	sent = n > 0
	if err != nil || n != len(payload)+1 {
		return uncertain(errors.New("desktop IPC request transmission failed"))
	}
	data, err := io.ReadAll(io.LimitReader(bufio.NewReader(connection), maxOutput+1))
	if err != nil || len(data) > maxOutput || !utf8.Valid(data) || !uniqueObject(data) {
		return uncertain(errors.New("desktop IPC reply is missing or invalid"))
	}
	var response appResponse
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil || response.Protocol != 1 || response.RequestID != request.RequestID {
		return uncertain(errors.New("desktop IPC reply identity mismatch"))
	}
	mode := "--" + request.Operation
	if request.Operation == "validate_target" || request.Operation == "inspect" {
		mode = "--inspect"
	}
	result, err := decodeResult(response.Result, mode)
	if err != nil {
		return uncertain(fmt.Errorf("desktop IPC result validation failed: %w", err))
	}
	if request.Operation != "resume" && result.Submitted {
		return uncertain(errors.New("read-only desktop request reported a submission"))
	}
	return result, response.Inspection, nil
}

func privateIPCToken(socket, tokenFile string) (string, error) {
	directory, err := os.Lstat(filepath.Dir(socket))
	if err != nil || !directory.IsDir() || directory.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(directory) {
		return "", errors.New("invalid IPC directory")
	}
	socketInfo, err := os.Lstat(socket)
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm()&0077 != 0 || !ownedByCurrentUser(socketInfo) {
		return "", errors.New("invalid IPC socket")
	}
	info, err := os.Lstat(tokenFile)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 128 || !ownedByCurrentUser(info) {
		return "", errors.New("invalid IPC token file")
	}
	file, err := openPrivateToken(tokenFile)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return "", errors.New("IPC token changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 32 || token != strings.ToLower(token) {
		return "", errors.New("invalid IPC credential")
	}
	return token, nil
}
