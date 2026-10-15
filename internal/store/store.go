package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"quota-session-orchestrator/internal/domain"
)

// State is changed only inside Update, which uses an immediate SQLite transaction.
// Hook timestamps are separate from scheduler timestamps to reject late hook events.
type State struct {
	Sessions        map[string]domain.Session `json:"sessions"`
	Jobs            map[string]domain.Job     `json:"jobs"`
	Accounts        []domain.Account          `json:"accounts"`
	Policy          *domain.Policy            `json:"policy,omitempty"`
	Capabilities    domain.Capabilities       `json:"capabilities"`
	Audit           []domain.Audit            `json:"audit"`
	RunningAccounts map[string]string         `json:"running_accounts"`
	// BoundSources is engine-owned identity affinity, never a client-supplied mapping.
	BoundSources    map[string]map[string]string `json:"bound_sources"`
	QuotaFailures   map[string]time.Time         `json:"quota_failures"`
	DispatchStarted map[string]time.Time         `json:"dispatch_started"`
	HookBarriers    map[string]time.Time         `json:"hook_barriers"`
	LastSend        time.Time                    `json:"last_send"`
	DesktopOwner    string                       `json:"desktop_owner"`
	DesktopUntil    time.Time                    `json:"desktop_until"`
	ResumeHistory   map[string][]time.Time       `json:"resume_history,omitempty"`
	RunningSince    map[string]time.Time         `json:"running_since,omitempty"`
	ControlGrants   map[string]ControlGrant      `json:"control_grants,omitempty"`
	ControlRequests map[string]ControlRequest    `json:"control_requests,omitempty"`
	// Signals is the state of each reset-signal source, keyed by source ID.
	Signals map[string]domain.SignalSourceState `json:"signals,omitempty"`
}

type Store struct{ db *sql.DB }
type Tx struct {
	*State
	tx  *sql.Tx
	ctx context.Context
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is required")
	}
	var dsn string
	if path == ":memory:" {
		dsn = ":memory:?_busy_timeout=5000&_foreign_keys=on&_txlock=immediate"
	} else {
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if err = os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
		if err = os.Chmod(abs, 0600); err != nil {
			return nil, err
		}
		u := url.URL{Scheme: "file", Path: abs}
		dsn = u.String() + "?_busy_timeout=5000&_foreign_keys=on&_journal_mode=WAL&_synchronous=FULL&_txlock=immediate"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version > schemaVersion {
		db.Close()
		return nil, errors.New("unsupported database schema version")
	}
	if version == 1 {
		if err = migrateJobsV2(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate store: %w", err)
		}
	}
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize store: %w", err)
	}
	return s, nil
}

const schemaVersion = 2

// Version 2: one resume job per session generation still holds, but explicit
// test sends may be repeated within a generation (no hook arrives when a test
// is refused before writing), so uniqueness no longer covers kind='test'.
const schema = `
CREATE TABLE IF NOT EXISTS scheduler_state (id INTEGER PRIMARY KEY CHECK(id=1), data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS quota_anchors (id INTEGER PRIMARY KEY CHECK(id=1), data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS quota_source_anchors (source_id TEXT PRIMARY KEY, data BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS hook_events (event_id TEXT PRIMARY KEY, session_id TEXT NOT NULL, observed_ns INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS hook_watermarks (session_id TEXT PRIMARY KEY, observed_ns INTEGER NOT NULL);
` + jobsTable + `
CREATE UNIQUE INDEX IF NOT EXISTS jobs_resume_generation ON jobs(session_id,generation) WHERE kind='';
CREATE INDEX IF NOT EXISTS jobs_due ON jobs(state,not_before_ns);
PRAGMA user_version=2;
`

const jobsTable = `CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, generation INTEGER NOT NULL, kind TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, not_before_ns INTEGER NOT NULL, lease_until_ns INTEGER, data BLOB NOT NULL);`

// migrateJobsV2 rebuilds the version 1 jobs table (table-level UNIQUE) in one
// transaction. Each row's kind is decoded in Go from its stored JSON.
func migrateJobsV2(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("ALTER TABLE jobs RENAME TO jobs_v1; DROP INDEX IF EXISTS jobs_due;"); err != nil {
		return err
	}
	if _, err = tx.Exec(jobsTable); err != nil {
		return err
	}
	rows, err := tx.Query("SELECT id,session_id,generation,state,not_before_ns,lease_until_ns,data FROM jobs_v1")
	if err != nil {
		return err
	}
	type row struct {
		id, session, state    string
		generation, notBefore int64
		lease                 sql.NullInt64
		data                  []byte
	}
	var all []row
	for rows.Next() {
		var r row
		if err = rows.Scan(&r.id, &r.session, &r.generation, &r.state, &r.notBefore, &r.lease, &r.data); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, r := range all {
		var j struct {
			Kind string `json:"kind"`
		}
		if err = json.Unmarshal(r.data, &j); err != nil {
			return fmt.Errorf("job %q: %w", r.id, err)
		}
		var lease any
		if r.lease.Valid {
			lease = r.lease.Int64
		}
		if _, err = tx.Exec("INSERT INTO jobs(id,session_id,generation,kind,state,not_before_ns,lease_until_ns,data) VALUES(?,?,?,?,?,?,?,?)", r.id, r.session, r.generation, j.Kind, r.state, r.notBefore, lease, r.data); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DROP TABLE jobs_v1; PRAGMA user_version=2;"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.db.Close() }

func empty() *State {
	return &State{Sessions: map[string]domain.Session{}, Jobs: map[string]domain.Job{}, Accounts: []domain.Account{}, Audit: []domain.Audit{}, RunningAccounts: map[string]string{}, BoundSources: map[string]map[string]string{}, QuotaFailures: map[string]time.Time{}, DispatchStarted: map[string]time.Time{}, HookBarriers: map[string]time.Time{}}
}

func load(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (*State, error) {
	st := empty()
	var data []byte
	err := q.QueryRowContext(ctx, "SELECT data FROM scheduler_state WHERE id=1").Scan(&data)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if err = json.Unmarshal(data, st); err != nil {
			return nil, err
		}
	}
	if st.Sessions == nil {
		st.Sessions = map[string]domain.Session{}
	}
	if st.RunningAccounts == nil {
		st.RunningAccounts = map[string]string{}
	}
	if st.BoundSources == nil {
		st.BoundSources = map[string]map[string]string{}
	}
	if st.QuotaFailures == nil {
		st.QuotaFailures = map[string]time.Time{}
	}
	if st.DispatchStarted == nil {
		st.DispatchStarted = map[string]time.Time{}
	}
	if st.HookBarriers == nil {
		st.HookBarriers = map[string]time.Time{}
	}
	st.Jobs = map[string]domain.Job{}
	rows, err := q.QueryContext(ctx, "SELECT data FROM jobs ORDER BY not_before_ns,id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		var j domain.Job
		if err = json.Unmarshal(b, &j); err != nil {
			return nil, err
		}
		st.Jobs[j.ID] = j
	}
	return st, rows.Err()
}

func (s *Store) Read(ctx context.Context) (*State, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	st, err := load(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := load(ctx, tx)
	if err != nil {
		return err
	}
	if err = fn(&Tx{State: st, tx: tx, ctx: ctx}); err != nil {
		return err
	}
	// The queue is durable, indexed and protected by a session/generation uniqueness constraint.
	jobs := st.Jobs
	st.Jobs = nil
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO scheduler_state(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data WHERE data<>excluded.data", b); err != nil {
		return err
	}
	for _, j := range jobs {
		b, err = json.Marshal(j)
		if err != nil {
			return err
		}
		var lease any
		if j.LeaseUntil != nil {
			lease = j.LeaseUntil.UnixNano()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,session_id,generation,kind,state,not_before_ns,lease_until_ns,data) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET state=excluded.state,not_before_ns=excluded.not_before_ns,lease_until_ns=excluded.lease_until_ns,data=excluded.data WHERE data<>excluded.data`, j.ID, j.SessionID, j.Generation, j.Kind, j.State, j.NotBefore.UnixNano(), lease, b)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// AcceptEvent records duplicates and late events without letting them change state.
// The caller must perform all hook mutations in this same transaction.
func (t *Tx) AcceptEvent(e domain.HookEvent) (bool, error) {
	r, err := t.tx.ExecContext(t.ctx, "INSERT OR IGNORE INTO hook_events(event_id,session_id,observed_ns) VALUES(?,?,?)", e.EventID, e.SessionID, e.ObservedAt.UnixNano())
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	var last int64
	err = t.tx.QueryRowContext(t.ctx, "SELECT observed_ns FROM hook_watermarks WHERE session_id=?", e.SessionID).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && e.ObservedAt.UnixNano() <= last {
		return false, nil
	}
	_, err = t.tx.ExecContext(t.ctx, "INSERT INTO hook_watermarks(session_id,observed_ns) VALUES(?,?) ON CONFLICT(session_id) DO UPDATE SET observed_ns=excluded.observed_ns", e.SessionID, e.ObservedAt.UnixNano())
	return err == nil, err
}

func (t *Tx) Log(now time.Time, kind, sessionID, message string) {
	var id int64 = 1
	if n := len(t.Audit); n > 0 {
		id = t.Audit[n-1].ID + 1
	}
	t.Audit = append(t.Audit, domain.Audit{ID: id, Time: now, Kind: kind, SessionID: sessionID, Message: message})
	if len(t.Audit) > 512 {
		t.Audit = t.Audit[len(t.Audit)-512:]
	}
}
