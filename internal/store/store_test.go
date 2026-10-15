package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"quota-session-orchestrator/internal/domain"
)

func TestWALPersistenceRollbackAndDedup(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var mode string
	if err = s.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode=%s err=%v", mode, err)
	}
	now := time.Now().UTC()
	event := domain.HookEvent{EventID: "one", SessionID: "s", ObservedAt: now}
	err = s.Update(ctx, func(tx *Tx) error {
		ok, err := tx.AcceptEvent(event)
		if err != nil || !ok {
			t.Fatalf("accepted=%v err=%v", ok, err)
		}
		tx.Sessions["s"] = domain.Session{ID: "s", Generation: 3}
		tx.Jobs["j"] = domain.Job{ID: "j", SessionID: "s", Generation: 3, State: domain.Dispatching, CreatedAt: now, UpdatedAt: now}
		tx.Log(now, "hook", "s", "SessionStart")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update(ctx, func(tx *Tx) error {
		tx.Sessions["s"] = domain.Session{ID: "s", Generation: 99}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, err := s.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Sessions["s"].Generation != 3 || state.Jobs["j"].State != domain.Dispatching || len(state.Audit) != 1 {
		t.Fatalf("state did not survive reopen: %+v", state)
	}
	err = s.Update(ctx, func(tx *Tx) error {
		ok, err := tx.AcceptEvent(event)
		if ok {
			t.Error("duplicate accepted")
		}
		if err != nil {
			return err
		}
		late := event
		late.EventID = "late"
		late.ObservedAt = now.Add(-time.Second)
		ok, err = tx.AcceptEvent(late)
		if ok {
			t.Error("late event accepted")
		}
		if err != nil {
			return err
		}
		newer := event
		newer.EventID = "newer"
		newer.ObservedAt = now.Add(time.Second)
		ok, err = tx.AcceptEvent(newer)
		if !ok {
			t.Error("newer event rejected")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAtomicUpdateAcrossHandles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.sqlite")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := a
			if i%2 == 0 {
				s = b
			}
			errs <- s.Update(context.Background(), func(tx *Tx) error {
				v := tx.Sessions["counter"]
				v.ID = "counter"
				v.Generation++
				tx.Sessions[v.ID] = v
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Sessions["counter"].Generation != 40 {
		t.Fatalf("lost update: %d", st.Sessions["counter"].Generation)
	}
}

func TestEventDedupRollsBackWithMutation(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	event := domain.HookEvent{EventID: "e", SessionID: "s", ObservedAt: time.Now()}
	_ = s.Update(context.Background(), func(tx *Tx) error {
		_, err := tx.AcceptEvent(event)
		if err != nil {
			return err
		}
		return errors.New("fail")
	})
	if err = s.Update(context.Background(), func(tx *Tx) error {
		accepted, err := tx.AcceptEvent(event)
		if !accepted {
			t.Error("rollback consumed event ID")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUniqueSessionGeneration(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Update(context.Background(), func(tx *Tx) error {
		for _, id := range []string{"a", "b"} {
			tx.Jobs[id] = domain.Job{ID: id, SessionID: "s", Generation: 1}
		}
		return nil
	})
	if err == nil {
		t.Fatal("duplicate session/generation queue row accepted")
	}
	st, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Jobs) != 0 {
		t.Fatal("partial transaction committed")
	}
}

func TestRepeatedTestSendsShareGeneration(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	err = s.Update(context.Background(), func(tx *Tx) error {
		tx.Jobs["s/1"] = domain.Job{ID: "s/1", SessionID: "s", Generation: 1}
		for _, id := range []string{"s/test-a", "s/test-b"} {
			tx.Jobs[id] = domain.Job{ID: id, SessionID: "s", Generation: 1, Kind: "test"}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("test sends in one generation rejected: %v", err)
	}
}

func TestMigrateVersion1Jobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE jobs (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, generation INTEGER NOT NULL, state TEXT NOT NULL, not_before_ns INTEGER NOT NULL, lease_until_ns INTEGER, data BLOB NOT NULL, UNIQUE(session_id,generation));
CREATE INDEX jobs_due ON jobs(state,not_before_ns);
INSERT INTO jobs VALUES('s/test-a','s',6,'NEEDS_ATTENTION',1,NULL,CAST('{"id":"s/test-a","session_id":"s","generation":6,"kind":"test"}' AS BLOB));
INSERT INTO jobs VALUES('r/2','r',2,'COOLDOWN',2,5,CAST('{"id":"r/2","session_id":"r","generation":2}' AS BLOB));
PRAGMA user_version=1;`)
	if err == nil {
		err = db.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version int
	if err = s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("version %d, %v", version, err)
	}
	st, err := s.Read(context.Background())
	if err != nil || len(st.Jobs) != 2 || st.Jobs["s/test-a"].Kind != "test" {
		t.Fatalf("migrated jobs %+v, %v", st.Jobs, err)
	}
	err = s.Update(context.Background(), func(tx *Tx) error {
		tx.Jobs["s/test-b"] = domain.Job{ID: "s/test-b", SessionID: "s", Generation: 6, Kind: "test"}
		return nil
	})
	if err != nil {
		t.Fatalf("repeat test send after migration: %v", err)
	}
	err = s.Update(context.Background(), func(tx *Tx) error {
		tx.Jobs["r/dup"] = domain.Job{ID: "r/dup", SessionID: "r", Generation: 2}
		return nil
	})
	if err == nil {
		t.Fatal("duplicate resume job accepted after migration")
	}
}
