package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"quota-session-orchestrator/internal/domain"
)

const (
	catalogMaxFiles = 5000
	catalogMaxBytes = 1 << 20
)

// catalogRoots are the Claude desktop session indexes (first-party and
// third-party inference builds keep separate data directories).
func catalogRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	base := filepath.Join(home, "Library", "Application Support")
	return []string{filepath.Join(base, "Claude-3p", "claude-code-sessions"), filepath.Join(base, "Claude", "claude-code-sessions")}
}

// readCatalog lists root/<account>/<profile>/local_<uuid>.json. Only the
// session's ids, title, model, folder, archive flag and last activity are
// decoded; every other field (prompts, summaries, permissions) is discarded by
// the decoder and never kept. complete is false when nothing could be read or
// a directory failed, so callers never treat a partial read as deletions.
func readCatalog(roots []string) (entries []domain.CatalogEntry, complete bool) {
	complete = true
	found := false
	files := 0
	for _, root := range roots {
		st, err := os.Lstat(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !st.IsDir() {
			complete = false
			continue
		}
		found = true
		for _, account := range catalogDirs(root, &complete) {
			for _, profile := range catalogDirs(account, &complete) {
				list, err := os.ReadDir(profile)
				if err != nil {
					complete = false
					continue
				}
				for _, item := range list {
					name := item.Name()
					if !item.Type().IsRegular() || !strings.HasPrefix(name, "local_") || !strings.HasSuffix(name, ".json") {
						continue
					}
					if files++; files > catalogMaxFiles {
						return entries, false
					}
					entry, ok := readCatalogFile(filepath.Join(profile, name))
					if !ok {
						continue
					}
					entries = append(entries, entry)
				}
			}
		}
	}
	return entries, complete && found
}

func catalogDirs(dir string, complete *bool) []string {
	list, err := os.ReadDir(dir)
	if err != nil {
		*complete = false
		return nil
	}
	var out []string
	for _, item := range list {
		if item.IsDir() && item.Type()&fs.ModeSymlink == 0 && !strings.HasPrefix(item.Name(), ".") {
			out = append(out, filepath.Join(dir, item.Name()))
		}
	}
	return out
}

func readCatalogFile(path string) (domain.CatalogEntry, bool) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() > catalogMaxBytes {
		return domain.CatalogEntry{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return domain.CatalogEntry{}, false
	}
	defer f.Close()
	var raw struct {
		SessionID      string  `json:"sessionId"`
		CLISessionID   string  `json:"cliSessionId"`
		Cwd            string  `json:"cwd"`
		Title          string  `json:"title"`
		Model          string  `json:"model"`
		IsArchived     bool    `json:"isArchived"`
		LastActivityAt float64 `json:"lastActivityAt"`
	}
	if err := json.NewDecoder(io.LimitReader(f, catalogMaxBytes)).Decode(&raw); err != nil {
		return domain.CatalogEntry{}, false
	}
	if filepath.Base(path) != raw.SessionID+".json" {
		return domain.CatalogEntry{}, false
	}
	entry := domain.CatalogEntry{HostSessionID: raw.SessionID, CLISessionID: raw.CLISessionID, Cwd: raw.Cwd,
		Title: cleanTitle(raw.Title), Model: strings.TrimSpace(raw.Model), Archived: raw.IsArchived}
	if raw.LastActivityAt > 0 && raw.LastActivityAt < 1e14 {
		entry.LastActivityAt = time.UnixMilli(int64(raw.LastActivityAt)).UTC()
	}
	if !domain.ValidCatalogEntry(entry) {
		return domain.CatalogEntry{}, false
	}
	return entry, true
}

type catalogEngine interface {
	SyncDesktopCatalog(context.Context, []domain.CatalogEntry, bool) error
}

// watchCatalog keeps the session list in step with Claude's sidebar.
func watchCatalog(ctx context.Context, e catalogEngine, roots []string) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if entries, complete := readCatalog(roots); len(entries) > 0 || complete {
			_ = e.SyncDesktopCatalog(ctx, entries, complete)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
