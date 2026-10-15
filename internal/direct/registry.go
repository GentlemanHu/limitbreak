// Package direct delivers a continue message to a Claude Code session through
// Claude's own cross-session messaging channel: a per-process unix socket
// advertised in the world-readable session registry. It reads only the
// registry's non-secret catalog metadata (socket path, session ids, liveness,
// status); it never opens the sibling per-session 0600 ".key" token files.
package direct

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Entry is the subset of a registry record this package relies on. Field names
// mirror the registry JSON; secret-bearing siblings (the ".key" files) are
// never read.
type Entry struct {
	PID           int    `json:"pid"`
	SessionID     string `json:"sessionId"`     // the CLI conversation uuid
	HostSessionID string `json:"hostSessionId"` // desktop "local_<uuid>" owner, when hosted
	Kind          string `json:"kind"`
	Entrypoint    string `json:"entrypoint"`
	SocketPath    string `json:"messagingSocketPath"`
	Status        string `json:"status"`          // "idle" | "busy"
	StatusUpdated int64  `json:"statusUpdatedAt"` // ms since epoch
	ProcStart     string `json:"procStart"`       // asctime, UTC
	Version       string `json:"version"`
	PeerProtocol  int    `json:"peerProtocol"`
}

// procStartLayout is the asctime form the registry writes, e.g.
// "Wed Oct 14 07:25:49 2026"; the value is the process start in UTC.
const procStartLayout = "Mon Jan _2 15:04:05 2006"

// ParseProcStart returns the recorded process start time, or false when the
// field is absent or in an unexpected shape.
func (e Entry) ParseProcStart() (time.Time, bool) {
	if e.ProcStart == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(procStartLayout, e.ProcStart, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// SessionsDirs lists the registry directories a running claude may write to:
// $CLAUDE_CONFIG_DIR/sessions when set, and always ~/.claude/sessions (Claude
// desktop, Claude 3P and the terminal CLI share it by default).
func SessionsDirs() []string {
	var dirs []string
	if dir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); dir != "" && filepath.IsAbs(dir) {
		dirs = append(dirs, filepath.Join(filepath.Clean(dir), "sessions"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		def := filepath.Join(home, ".claude", "sessions")
		if len(dirs) == 0 || dirs[0] != def {
			dirs = append(dirs, def)
		}
	}
	return dirs
}

// ownedBy reports whether info is a non-symlink owned by uid that neither the
// group nor others may write.
func ownedBy(info os.FileInfo, uid int) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == uid && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 == 0
}

// readRegistry returns every parseable "<pid>.json" record in dir. ".key" files
// and any entry that fails to parse are skipped silently; a single malformed
// peer never blocks delivery to a healthy one. Records not owned by uid, in a
// directory another user could write to, are never trusted.
func readRegistry(dir string, uid int) ([]Entry, error) {
	if dir == "" {
		return nil, os.ErrNotExist
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || !ownedBy(info, uid) {
		return nil, os.ErrPermission
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(names))
	for _, name := range names {
		if fi, err := os.Lstat(name); err != nil || !fi.Mode().IsRegular() || !ownedBy(fi, uid) {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil || len(data) == 0 || len(data) > 1<<20 {
			continue
		}
		var e Entry
		if json.Unmarshal(data, &e) != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}
