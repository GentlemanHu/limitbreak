//go:build darwin || linux || freebsd

package bridge

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func ipcFixture(t *testing.T, handler func(net.Conn, appRequest)) (*AppIPC, string) {
	t.Helper()
	root := os.Getenv("TMPDIR")
	if root == "" {
		t.Fatal("IPC tests require explicit TMPDIR")
	}
	dir, err := os.MkdirTemp(root, "qsi-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket, tokenFile := filepath.Join(dir, "s"), filepath.Join(dir, "t")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(strings.Repeat("a", 64)), 0600); err != nil {
		t.Fatal(err)
	}
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		line, err := bufio.NewReader(conn).ReadBytes('\n')
		if err != nil {
			return
		}
		var request appRequest
		if json.Unmarshal(line, &request) != nil {
			return
		}
		handler(conn, request)
	}()
	return NewAppIPC(socket, tokenFile), dir
}

func ipcReply(conn net.Conn, r appRequest, result domain.BridgeResult) {
	data, _ := json.Marshal(result)
	_ = json.NewEncoder(conn).Encode(appResponse{Protocol: 1, RequestID: r.RequestID, Result: data})
}

func ipcBinding() domain.Binding {
	return domain.Binding{SessionID: "authorized-target", Kind: "code", Provider: "anthropic", Model: "opus", AccountIDs: []string{"account"}, TargetIdentifier: "authorized-target", ProjectMarker: "isolated-project", AutoResume: true, AcknowledgeSideEffects: true, ResumeOwner: "guard", NativeAutoResumeDisabledAcknowledged: true, ExecutionGeneration: 4, ExecutionLease: "engine-owner/authorized-target/4", ExecutionUserIdleSeconds: 3}
}

func TestAppIPCAuthenticatedResumeAndProbe(t *testing.T) {
	seen := make(chan appRequest, 1)
	client, _ := ipcFixture(t, func(conn net.Conn, r appRequest) {
		seen <- r
		ipcReply(conn, r, domain.BridgeResult{OK: true, Submitted: true, Code: "submitted", AccessibilityGranted: true})
	})
	result, err := client.Resume(context.Background(), ipcBinding(), "继续完成任务")
	if err != nil || !result.OK || !result.Submitted {
		t.Fatalf("resume: %+v %v", result, err)
	}
	r := <-seen
	want := sha256.Sum256([]byte("继续完成任务"))
	if r.Protocol != 1 || r.Token != strings.Repeat("a", 64) || len(r.RequestID) != 32 || r.Generation != 4 || r.LeaseID != "engine-owner/authorized-target/4" || r.UserIdleSeconds != 3 || r.MessageHash != hex.EncodeToString(want[:]) {
		t.Fatal("request identity/authorization missing")
	}
	if _, err := time.Parse(time.RFC3339Nano, r.Deadline); err != nil {
		t.Fatal("invalid deadline")
	}
	wire, _ := json.Marshal(r.Binding)
	if strings.Contains(string(wire), "execution_") {
		t.Fatal("internal execution fields leaked into binding")
	}

	probe, _ := ipcFixture(t, func(conn net.Conn, r appRequest) {
		ipcReply(conn, r, domain.BridgeResult{OK: true, Code: "ready", AccessibilityGranted: true})
	})
	if result := probe.Probe(context.Background()); !result.OK || result.Submitted {
		t.Fatalf("probe: %+v", result)
	}
}

func TestAppIPCUncertainResponsesNeverLookRetryable(t *testing.T) {
	for _, failure := range []string{"missing", "wrong_identity", "duplicate", "oversized"} {
		t.Run(failure, func(t *testing.T) {
			client, _ := ipcFixture(t, func(conn net.Conn, r appRequest) {
				switch failure {
				case "missing":
					return
				case "wrong_identity":
					r.RequestID = "wrong"
					ipcReply(conn, r, domain.BridgeResult{OK: true, Submitted: true, Code: "submitted", AccessibilityGranted: true})
				case "duplicate":
					_, _ = io.WriteString(conn, `{"protocol":1,"protocol":1,"request_id":"`+r.RequestID+`","result":{}}`)
				case "oversized":
					_, _ = io.WriteString(conn, strings.Repeat(" ", maxOutput+1))
				}
			})
			result, err := client.Resume(context.Background(), ipcBinding(), "继续")
			if err == nil || result.OK || !result.Submitted || result.Code != "submitted_uncertain" {
				t.Fatalf("retryable result after handoff: %+v %v", result, err)
			}
		})
	}
}

func TestAppIPCCancellationClosesConnection(t *testing.T) {
	entered, closed := make(chan struct{}), make(chan struct{})
	client, _ := ipcFixture(t, func(conn net.Conn, r appRequest) { close(entered); _, _ = io.Copy(io.Discard, conn); close(closed) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan domain.BridgeResult, 1)
	go func() { result, _ := client.Resume(ctx, ipcBinding(), "继续"); done <- result }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("IPC not reached")
	}
	cancel()
	select {
	case result := <-done:
		if !result.Submitted || result.Code != "submitted_uncertain" {
			t.Fatal("cancelled handed-off send became retryable")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not interrupt socket")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("app peer did not receive EOF")
	}
}

func TestAppIPCPrivateIdentityAndOwnershipRequired(t *testing.T) {
	client, dir := ipcFixture(t, func(conn net.Conn, r appRequest) {
		ipcReply(conn, r, domain.BridgeResult{OK: true, Code: "ready", AccessibilityGranted: true})
	})
	if err := os.Chmod(filepath.Join(dir, "t"), 0644); err != nil {
		t.Fatal(err)
	}
	if result := client.Probe(context.Background()); result.OK || result.Code != "bridge_unavailable" {
		t.Fatal("wide token permissions accepted")
	}
	if err := os.Chmod(filepath.Join(dir, "t"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "t"), filepath.Join(dir, "real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "t")); err != nil {
		t.Fatal(err)
	}
	if result := client.Probe(context.Background()); result.OK {
		t.Fatal("token symlink accepted")
	}
	for _, owner := range []string{"", "native", "manual", "unknown"} {
		b := ipcBinding()
		b.ResumeOwner = owner
		result, _ := client.Resume(context.Background(), b, "继续")
		if result.Submitted || result.Code != "consent_required" {
			t.Fatal("unowned resume transmitted")
		}
	}
}

func TestAppIPCDeepLinkHostedResumeAndLocate(t *testing.T) {
	link := "claude://claude.ai/epitaxy/local_0123abcd-0123-4567-89ab-0123456789ab"
	hosted := domain.Binding{SessionID: "hosted", Kind: "code", Executor: domain.ExecutorDesktop, DeepLink: link, TargetTitle: "测试会话",
		ResumeOwner: "auto", Origin: &domain.SessionOrigin{App: domain.AppClaudeDesktop, ClaudePID: 42}, TargetOverride: "desktop", Inferred: true,
		ExecutionGeneration: 2, ExecutionLease: "owner/job/2"}
	seen := make(chan appRequest, 1)
	client, _ := ipcFixture(t, func(conn net.Conn, r appRequest) {
		seen <- r
		ipcReply(conn, r, domain.BridgeResult{OK: true, Submitted: true, Code: "submitted", AccessibilityGranted: true})
	})
	if result, err := client.Resume(context.Background(), hosted, "继续"); err != nil || !result.OK {
		t.Fatalf("hosted resume: %+v %v", result, err)
	}
	r := <-seen
	wire, _ := json.Marshal(r.Binding)
	for _, leak := range []string{"origin", "target_override", "inferred", "claude_pid"} {
		if strings.Contains(string(wire), leak) {
			t.Fatalf("daemon-only field %q sent to App: %s", leak, wire)
		}
	}
	if r.Binding.DeepLink != link || r.Binding.Executor != domain.ExecutorDesktop {
		t.Fatal("deep link target not transmitted")
	}

	for _, bad := range []string{"", "claude://claude.ai/epitaxy/local_x", "https://claude.ai/epitaxy/local_0123abcd-0123-4567-89ab-0123456789ab"} {
		b := hosted
		b.DeepLink = bad
		if result, _ := client.Resume(context.Background(), b, "继续"); result.Submitted || result.Code != "consent_required" {
			t.Fatalf("invalid deep link %q accepted", bad)
		}
		if result := client.Locate(context.Background(), b); result.OK || result.Code != "locate_unavailable" {
			t.Fatalf("invalid deep link %q located", bad)
		}
	}

	locator, _ := ipcFixture(t, func(conn net.Conn, r appRequest) {
		if r.Operation != "locate" || r.Message != "" || r.Generation != 0 {
			return
		}
		ipcReply(conn, r, domain.BridgeResult{OK: true, Code: "located", AccessibilityGranted: true})
	})
	if result := locator.Locate(context.Background(), hosted); !result.OK || result.Submitted {
		t.Fatalf("locate: %+v", result)
	}
}

func TestAppIPCWakeCarriesLeaseAndAttentionSettings(t *testing.T) {
	link := "claude://claude.ai/epitaxy/local_0123abcd-0123-4567-89ab-0123456789ab"
	hosted := domain.Binding{SessionID: "hosted", Kind: "code", Executor: domain.ExecutorDesktop, DeepLink: link, TargetTitle: "测试会话",
		ResumeOwner: "auto", ExecutionGeneration: 3, ExecutionLease: "owner/job/3", ExecutionQuietSeconds: 120, ExecutionRestoreFocus: true}
	seen := make(chan appRequest, 1)
	client, _ := ipcFixture(t, func(conn net.Conn, r appRequest) {
		seen <- r
		ipcReply(conn, r, domain.BridgeResult{OK: true, Code: "woken"})
	})
	if result := client.Wake(context.Background(), hosted, "继续"); !result.OK || result.Submitted || result.Code != "woken" {
		t.Fatalf("wake: %+v", result)
	}
	r := <-seen
	want := sha256.Sum256([]byte("继续"))
	if r.Operation != "wake" || r.Message != "" || r.Generation != 3 || r.LeaseID != "owner/job/3" || r.MessageHash != hex.EncodeToString(want[:]) ||
		r.QuietSeconds != 120 || !r.RestoreFocus || r.UserIdleSeconds != 3 {
		t.Fatalf("wake request: %+v", r)
	}

	// A wake must never be reported as a submission.
	lying, _ := ipcFixture(t, func(conn net.Conn, r appRequest) {
		ipcReply(conn, r, domain.BridgeResult{OK: true, Submitted: true, Code: "submitted", AccessibilityGranted: true})
	})
	if result := lying.Wake(context.Background(), hosted, "继续"); result.OK || result.Submitted {
		t.Fatalf("submitting wake accepted: %+v", result)
	}
	plain := hosted
	plain.DeepLink = ""
	if result := client.Wake(context.Background(), plain, "继续"); result.OK || result.Code != "wake_unavailable" {
		t.Fatalf("wake without link: %+v", result)
	}
}
