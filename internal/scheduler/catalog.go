package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"quota-session-orchestrator/internal/domain"
	"quota-session-orchestrator/internal/store"
)

const reasonFromCatalog = "已从 Claude 桌面会话列表载入；尚未收到该会话的事件"

// SyncDesktopCatalog merges the Claude desktop session list into the guard's
// sessions: every desktop Code session becomes visible with its real title,
// model and routing id, before any hook event arrives. complete reports that
// the whole list was read, so sessions missing from it may be marked removed.
func (e *Engine) SyncDesktopCatalog(ctx context.Context, entries []domain.CatalogEntry, complete bool) error {
	if err := e.init(ctx); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	changed := false
	err := e.store.Update(ctx, func(t *store.Tx) error {
		changed = mergeCatalog(t, entries, complete, e.now())
		return nil
	})
	if err == nil && changed {
		e.signal()
	}
	return err
}

func mergeCatalog(t *store.Tx, entries []domain.CatalogEntry, complete bool, now time.Time) bool {
	listed := map[string]bool{}
	hostOwner := map[string]string{}
	added, changed := 0, false
	for _, c := range entries {
		if !domain.ValidCatalogEntry(c) || listed[c.CLISessionID] {
			continue
		}
		listed[c.CLISessionID] = true
		hostOwner[c.HostSessionID] = c.CLISessionID
		s, known := t.Sessions[c.CLISessionID]
		before, _ := json.Marshal(s)
		if !known {
			s = domain.Session{ID: c.CLISessionID, Kind: "code", State: domain.Completed, Reason: reasonFromCatalog, UpdatedAt: c.LastActivityAt}
			if s.UpdatedAt.IsZero() {
				s.UpdatedAt = now
			}
			added++
		}
		applyCatalog(&s, c)
		if after, _ := json.Marshal(s); string(after) != string(before) {
			t.Sessions[s.ID] = s
			changed = true
		}
	}
	for id, s := range t.Sessions {
		before := s
		switch {
		case s.Origin == nil || s.Origin.App != domain.AppClaudeDesktop:
			continue
		case s.InCatalog && !listed[id] && complete:
			// Deleted from Claude: kept for its history, hidden from the list.
			s.InCatalog, s.Archived = false, true
		case !listed[id] && hostOwner[s.Origin.HostSessionID] != "" && !hasOpenJob(t, id):
			// The desktop session now runs a newer CLI conversation id.
			s.Archived = true
		}
		if s.InCatalog != before.InCatalog || s.Archived != before.Archived {
			t.Sessions[id] = s
			changed = true
		}
	}
	if added > 0 {
		t.Log(now, "catalog", "", fmt.Sprintf("已从 Claude 桌面会话列表载入 %d 个会话", added))
	}
	return changed
}

// applyCatalog copies the catalog's metadata. The catalog is Claude's own
// sidebar, so its title, model and session link are authoritative for desktop
// sessions; a session last seen in a terminal keeps its terminal origin.
func applyCatalog(s *domain.Session, c domain.CatalogEntry) {
	s.InCatalog, s.Archived = true, c.Archived
	if !c.LastActivityAt.IsZero() {
		at := c.LastActivityAt.UTC()
		s.LastActivityAt = &at
	}
	if s.Kind == "" {
		s.Kind = "code"
	}
	if s.Cwd == "" && c.Cwd != "" {
		s.Cwd = c.Cwd
	}
	if s.Origin != nil && s.Origin.App == domain.AppTerminal {
		if s.Model == "" {
			s.Model = c.Model
		}
		return
	}
	if c.Title != "" {
		s.Title = c.Title
	}
	if c.Model != "" {
		s.Model = c.Model
	}
	if s.Origin == nil || s.Origin.App != domain.AppClaudeDesktop || s.Origin.HostSessionID != c.HostSessionID {
		origin := domain.SessionOrigin{App: domain.AppClaudeDesktop, BundleID: domain.DesktopBundleID, ObservedAt: c.LastActivityAt.UTC()}
		if s.Origin != nil {
			origin = *s.Origin
			origin.App = domain.AppClaudeDesktop
		}
		origin.HostSessionID = c.HostSessionID
		s.Origin = &origin
	}
}

func hasOpenJob(t *store.Tx, id string) bool {
	for _, j := range t.Jobs {
		if j.SessionID == id && (j.State == domain.Cooldown || j.State == domain.Ready || j.State == domain.Dispatching || j.State == domain.Verifying) {
			return true
		}
	}
	return false
}
