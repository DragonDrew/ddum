// Package store owns the bot's SQLite database: connections, migrations,
// transactions, and repositories.
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

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/DragonDrew/ddum/internal/ids"
	"github.com/DragonDrew/ddum/migrations"
)

// TimeFormat is the stored timestamp format: RFC 3339, UTC, fixed
// millisecond precision so that text comparison matches time order.
const TimeFormat = "2006-01-02T15:04:05.000Z"

// FormatTime converts t to the stored representation.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// ParseTime parses a stored timestamp.
func ParseTime(s string) (time.Time, error) { return time.Parse(TimeFormat, s) }

// Store holds two connection pools over one database file: a single-connection
// writer (all writes are serialized) and a reader pool.
type Store struct {
	w   *sql.DB
	r   *sql.DB
	ids ids.Generator
}

// Open opens (creating if needed) the database at path and applies all
// pending migrations.
func Open(ctx context.Context, path string, gen ids.Generator) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}
	if err := privateFiles(path); err != nil {
		return nil, err
	}
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxLifetime(0)

	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)

	s := &Store{w: w, r: r, ids: gen}
	if err := s.migrate(ctx, path); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds a modernc.org/sqlite DSN with the pragmas required on every
// connection. Writers take the lock at BEGIN so that
// read-then-write transactions cannot fail with SQLITE_BUSY halfway through.
func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Set("_txlock", "immediate")
	}
	return path + "?" + q.Encode()
}

// privateFiles makes the database readable by the bot only: it holds the bot's
// signing key and a full inventory of what runs where. SQLite gives the -wal and
// -shm files the database's mode, so an empty file is created first. A file
// from an earlier version is tightened, along with its companions.
func privateFiles(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create database: %w", err)
	}
	f.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("protect database: %w", err)
		}
	}
	return nil
}

// migrate applies pending migrations, first copying a database that already
// has data to <path>.before-schema-<version>. If a migration or the release that
// brought it goes wrong, that copy is the way back.
func (s *Store) migrate(ctx context.Context, path string) error {
	p, err := goose.NewProvider(goose.DialectSQLite3, s.w, migrations.FS)
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}
	current, err := p.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	pending, err := p.HasPending(ctx)
	if err != nil {
		return fmt.Errorf("look for pending migrations: %w", err)
	}
	if pending && current > 0 {
		copyPath := fmt.Sprintf("%s.before-schema-%d", path, current+1)
		if err := os.Remove(copyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replace %s: %w", copyPath, err)
		}
		if _, err := s.w.ExecContext(ctx, `VACUUM INTO ?`, copyPath); err != nil {
			return fmt.Errorf("back up before migrating: %w", err)
		}
		if err := os.Chmod(copyPath, 0o600); err != nil {
			return fmt.Errorf("protect %s: %w", copyPath, err)
		}
	}
	if _, err := p.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// SchemaVersion returns the highest applied migration version.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	p, err := goose.NewProvider(goose.DialectSQLite3, s.w, migrations.FS)
	if err != nil {
		return 0, err
	}
	return p.GetDBVersion(ctx)
}

// Close closes both pools.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

// Ping checks that the database answers queries.
func (s *Store) Ping(ctx context.Context) error {
	return s.r.PingContext(ctx)
}

// Reader returns the read-only pool for queries outside a transaction.
func (s *Store) Reader() *sql.DB { return s.r }

// WriteTx runs fn in a write transaction, committing if fn returns nil.
// Every state change and its audit event must go through one WriteTx
// .
func (s *Store) WriteTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
		if err != nil {
			tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// AuditEvent is one row of audit_events.
type AuditEvent struct {
	At             time.Time
	ActorKind      string // user | policy | agent | system
	ActorDiscordID string
	ActorName      string
	Action         string // e.g. "host.approved"
	TargetType     string
	TargetID       string
	Details        any // marshalled to details_json; nil means {}
	Result         string
}

// InsertAudit writes an audit event inside tx and returns its ID.
func (s *Store) InsertAudit(ctx context.Context, tx *sql.Tx, e AuditEvent) (string, error) {
	details := []byte("{}")
	if e.Details != nil {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return "", fmt.Errorf("audit details: %w", err)
		}
		details = b
	}
	if e.Result == "" {
		e.Result = "ok"
	}
	id := s.ids.New()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO audit_events (id, at, actor_kind, actor_discord_id, actor_name, action,
		                          target_type, target_id, details_json, result)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, FormatTime(e.At), e.ActorKind, nullString(e.ActorDiscordID), nullString(e.ActorName),
		e.Action, nullString(e.TargetType), nullString(e.TargetID), string(details), e.Result)
	if err != nil {
		return "", fmt.Errorf("insert audit event: %w", err)
	}
	return id, nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// Backup writes a consistent copy of the database to path with VACUUM INTO,
// which is safe while the bot runs. path must not exist.
func Backup(ctx context.Context, dbPath, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	db, err := sql.Open("sqlite", dsn(dbPath, false))
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("open %s: %w", dbPath, err)
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return os.Chmod(path, 0o600)
}

// OperationEventRetention is how long operation events are kept.
const OperationEventRetention = 180 * 24 * time.Hour

// PruneOperationEvents deletes operation events received before cutoff; the
// operation rows themselves are kept forever.
func (s *Store) PruneOperationEvents(ctx context.Context, cutoff time.Time) (int64, error) {
	var n int64
	err := s.WriteTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM operation_events WHERE received_at < ?`, FormatTime(cutoff))
		if err != nil {
			return err
		}
		n, _ = r.RowsAffected()
		return nil
	})
	return n, err
}
