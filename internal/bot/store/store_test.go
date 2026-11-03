package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// seqIDs is a deterministic ids.Generator for tests.
type seqIDs struct{ n atomic.Int64 }

func (g *seqIDs) New() string {
	return fmt.Sprintf("00000000-0000-7000-8000-%012d", g.n.Add(1)+1000)
}

const now = "2026-10-01T02:15:00.000Z"

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "sub", "ddum.db"), &seqIDs{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func exec(t *testing.T, s *Store, query string, args ...any) error {
	t.Helper()
	return s.WriteTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	})
}

func mustExec(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if err := exec(t, s, query, args...); err != nil {
		t.Fatalf("%v\n%s", err, query)
	}
}

// fixture inserts agent a1, host h1, stack s1 and container c1.
func fixture(t *testing.T, s *Store) {
	t.Helper()
	mustExec(t, s, `INSERT INTO agents (id, display_name, first_seen_at) VALUES ('a1','agent',?)`, now)
	mustExec(t, s, `INSERT INTO hosts (id, agent_id, display_name, created_at) VALUES ('h1','a1','host-3',?)`, now)
	mustExec(t, s, `INSERT INTO stacks (id, host_id, display_name, fingerprint, provider, created_at, updated_at)
		VALUES ('s1','h1','immich','compose:immich','compose',?,?)`, now, now)
	mustExec(t, s, `INSERT INTO containers (id, stack_id, identity_key, display_name, created_at, updated_at)
		VALUES ('c1','s1','immich-server','immich-server',?,?)`, now, now)
}

func TestDatabaseFilesArePrivateAndMigrationsBackUpFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no file modes on Windows")
	}
	path := filepath.Join(t.TempDir(), "ddum.db")
	s, err := Open(context.Background(), path, &seqIDs{})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", st.Mode().Perm(), err)
	}
	if _, err := os.Stat(path + ".before-schema-1"); err == nil {
		t.Fatal("a new database has nothing to back up")
	}
	s.Close()
	// An older database with a loose mode: pretend one migration is pending.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM goose_db_version WHERE version_id = (SELECT MAX(version_id) FROM goose_db_version)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	// Applying the newest migration again fails on its existing columns, which
	// does not matter here: the copy is made before it runs.
	if s, err := Open(context.Background(), path, &seqIDs{}); err == nil {
		s.Close()
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", st.Mode().Perm(), err)
	}
	if st, err := os.Stat(path + ".before-schema-7"); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("no private copy before migrating: %v", err)
	}
}

func TestMigrationsApplyToEmptyDatabase(t *testing.T) {
	s := openTest(t)
	v, err := s.SchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != 7 {
		t.Fatalf("schema version = %d, want 7", v)
	}
	for _, table := range []string{"agents", "hosts", "stacks", "containers", "update_groups",
		"update_group_members", "policies", "operations", "operation_events", "updates",
		"approval_offers", "snoozes", "desired_states", "rollback_snapshots", "audit_events",
		"discord_messages", "system_settings", "registry_tag_lists"} {
		var n int
		err := s.Reader().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s missing (err=%v)", table, err)
		}
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ddum.db")
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), path, &seqIDs{})
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		s.Close()
	}
}

func TestPragmas(t *testing.T) {
	s := openTest(t)
	var mode string
	if err := s.Reader().QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Errorf("journal_mode = %q (err=%v), want wal", mode, err)
	}
	for _, db := range []*sql.DB{s.w, s.r} {
		var fk, timeout int
		db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
		db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout)
		if fk != 1 || timeout != 5000 {
			t.Errorf("foreign_keys=%d busy_timeout=%d", fk, timeout)
		}
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s := openTest(t)
	err := exec(t, s, `INSERT INTO hosts (id, agent_id, display_name, created_at) VALUES ('h1','nope','x',?)`, now)
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("got %v, want foreign key violation", err)
	}
}

func TestReaderIsReadOnly(t *testing.T) {
	s := openTest(t)
	_, err := s.Reader().Exec(`INSERT INTO system_settings (key, value_json, updated_at) VALUES ('k','1',?)`, now)
	if err == nil {
		t.Fatal("write through the reader pool succeeded")
	}
}

func TestGlobalPolicySeeded(t *testing.T) {
	s := openTest(t)
	var mode, track string
	var timeout, stable int
	var rollback sql.NullInt64
	err := s.Reader().QueryRow(`SELECT mode, tag_track, auto_rollback, verify_timeout_seconds, verify_stable_seconds
		FROM policies WHERE scope_type='global'`).Scan(&mode, &track, &rollback, &timeout, &stable)
	if err != nil {
		t.Fatal(err)
	}
	if mode != "manual" || track != "same-major" || rollback.Valid || timeout != 120 || stable != 30 {
		t.Fatalf("global policy = %s %s %v %d %d", mode, track, rollback, timeout, stable)
	}
	if err := exec(t, s, `INSERT INTO policies (id, scope_type, scope_id, updated_at) VALUES ('p2','global',NULL,?)`, now); err == nil {
		t.Fatal("second global policy accepted")
	}
	if err := exec(t, s, `INSERT INTO policies (id, scope_type, scope_id, updated_at) VALUES ('p3','host',NULL,?)`, now); err == nil {
		t.Fatal("non-global policy without scope_id accepted")
	}
}

func TestAuditEventsImmutable(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var id string
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		var err error
		id, err = s.InsertAudit(ctx, tx, AuditEvent{At: time.Now(), ActorKind: "system", Action: "test.event"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`UPDATE audit_events SET action='changed' WHERE id=?`,
		`DELETE FROM audit_events WHERE id=?`,
	} {
		if err := exec(t, s, q, id); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Errorf("%s: got %v, want immutable error", q, err)
		}
	}
}

func TestOperationEventsImmutableButDeletable(t *testing.T) {
	s := openTest(t)
	fixture(t, s)
	mustExec(t, s, `INSERT INTO operations (id, kind, agent_id, host_id, scope_type, scope_id, payload_json,
		state, expires_at, initiator_kind, created_at, updated_at)
		VALUES ('o1','deploy','a1','h1','stack','s1','{}','queued',?,'user',?,?)`, now, now, now)
	mustExec(t, s, `INSERT INTO operation_events (id, operation_id, seq, type, received_at)
		VALUES ('e1','o1',1,'accepted',?)`, now)

	if err := exec(t, s, `UPDATE operation_events SET type='failed' WHERE id='e1'`); err == nil {
		t.Fatal("operation_events update succeeded")
	}
	// Duplicate (operation_id, seq) is rejected so retried events de-duplicate.
	if err := exec(t, s, `INSERT INTO operation_events (id, operation_id, seq, type, received_at)
		VALUES ('e2','o1',1,'accepted',?)`, now); err == nil {
		t.Fatal("duplicate seq accepted")
	}
	// The retention job may delete old rows.
	mustExec(t, s, `DELETE FROM operation_events WHERE id='e1'`)
}

func TestUniquenessConstraints(t *testing.T) {
	s := openTest(t)
	fixture(t, s)
	cases := map[string]string{
		"stack fingerprint per host": `INSERT INTO stacks (id, host_id, display_name, fingerprint, provider, created_at, updated_at)
			VALUES ('s2','h1','other','compose:immich','compose','` + now + `','` + now + `')`,
		"container identity per stack": `INSERT INTO containers (id, stack_id, identity_key, display_name, created_at, updated_at)
			VALUES ('c2','s1','immich-server','dup','` + now + `','` + now + `')`,
		"one host per agent": `INSERT INTO hosts (id, agent_id, display_name, created_at) VALUES ('h2','a1','x','` + now + `')`,
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			if err := exec(t, s, q); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
				t.Fatalf("got %v, want UNIQUE violation", err)
			}
		})
	}

	t.Run("update per container and digest", func(t *testing.T) {
		ins := `INSERT INTO updates (id, container_id, stack_id, host_id, kind, current_image_ref, target_image_ref,
			target_digest, status, detected_at, updated_at) VALUES (?, 'c1','s1','h1','digest','a','b','sha256:x','detected',?,?)`
		mustExec(t, s, ins, "u1", now, now)
		if err := exec(t, s, ins, "u2", now, now); err == nil {
			t.Fatal("duplicate (container_id, target_digest) accepted")
		}
	})

	t.Run("one pending desired state per container", func(t *testing.T) {
		ins := `INSERT INTO desired_states (id, container_id, image_ref, image_digest, status, set_at)
			VALUES (?, 'c1', 'img', ?, ?, ?)`
		mustExec(t, s, ins, "d1", "sha256:a", "reached", now)
		mustExec(t, s, ins, "d2", "sha256:b", "reached", now)
		mustExec(t, s, ins, "d3", "sha256:c", "pending", now)
		if err := exec(t, s, ins, "d4", "sha256:d", "pending", now); err == nil {
			t.Fatal("second pending desired state accepted")
		}
	})
}

// A state change and its audit event commit or roll back together.
func TestWriteTxAuditIsTransactional(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	boom := errors.New("boom")

	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO system_settings (key, value_json, updated_at) VALUES ('maintenance','true',?)`, now); err != nil {
			return err
		}
		if _, err := s.InsertAudit(ctx, tx, AuditEvent{At: time.Now(), ActorKind: "user",
			ActorDiscordID: "123456789012345678", Action: "maintenance.on", Details: map[string]any{"on": true}}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v, want boom", err)
	}
	var settings, audits int
	s.Reader().QueryRow(`SELECT count(*) FROM system_settings`).Scan(&settings)
	s.Reader().QueryRow(`SELECT count(*) FROM audit_events`).Scan(&audits)
	if settings != 0 || audits != 0 {
		t.Fatalf("rollback left %d settings and %d audit rows", settings, audits)
	}

	// The same work without the error commits both rows.
	err = s.WriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO system_settings (key, value_json, updated_at) VALUES ('maintenance','true',?)`, now); err != nil {
			return err
		}
		_, err := s.InsertAudit(ctx, tx, AuditEvent{At: time.Now(), ActorKind: "user", Action: "maintenance.on"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var details, result string
	s.Reader().QueryRow(`SELECT details_json, result FROM audit_events`).Scan(&details, &result)
	if details != "{}" || result != "ok" {
		t.Fatalf("defaults: details=%q result=%q", details, result)
	}
}

func TestTimeFormatSortsChronologically(t *testing.T) {
	a := time.Date(2026, 10, 1, 2, 15, 0, 0, time.UTC)
	b := a.Add(time.Millisecond)
	c := a.Add(time.Second)
	if !(FormatTime(a) < FormatTime(b) && FormatTime(b) < FormatTime(c)) {
		t.Fatalf("not ordered: %s %s %s", FormatTime(a), FormatTime(b), FormatTime(c))
	}
	back, err := ParseTime(FormatTime(b))
	if err != nil || !back.Equal(b) {
		t.Fatalf("round trip: %v %v", back, err)
	}
}

func TestBackupAndRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ddum.db")
	s, err := Open(context.Background(), path, &seqIDs{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fixture(t, s)
	mustExec(t, s, `INSERT INTO operations (id, kind, agent_id, host_id, scope_type, scope_id, payload_json, state, expires_at,
		initiator_kind, created_at, updated_at) VALUES ('o1','deploy','a1','h1','stack','s1','{}','succeeded',?, 'user', ?, ?)`, now, now, now)
	mustExec(t, s, `INSERT INTO operation_events (id, operation_id, seq, type, received_at) VALUES ('e1','o1',1,'accepted','2026-01-01T00:00:00.000Z'),
		('e2','o1',2,'completed',?)`, now)

	backup := filepath.Join(dir, "backup.db")
	if err := Backup(context.Background(), path, backup); err != nil {
		t.Fatal(err)
	}
	if err := Backup(context.Background(), path, backup); err == nil {
		t.Fatal("an existing backup file must not be overwritten")
	}
	b, err := Open(context.Background(), backup, &seqIDs{})
	if err != nil {
		t.Fatal(err)
	}
	var hosts int
	b.Reader().QueryRow(`SELECT count(*) FROM hosts`).Scan(&hosts)
	b.Close()
	if hosts != 1 {
		t.Fatalf("backup hosts = %d", hosts)
	}

	cut, _ := ParseTime(now)
	n, err := s.PruneOperationEvents(context.Background(), cut.Add(-OperationEventRetention))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v; want the old event only", n, err)
	}
	var ops int
	s.Reader().QueryRow(`SELECT count(*) FROM operations`).Scan(&ops)
	if ops != 1 {
		t.Fatal("operations are retained forever")
	}
}
