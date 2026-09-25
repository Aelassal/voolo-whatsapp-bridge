// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Package store owns the bridge's session store: whatsmeow's sqlstore on the
// pure-Go SQLite of github.com/ncruces/go-sqlite3, encrypted with its Adiantum
// VFS (PROTOCOL.md §10.1, ADR-017 §2). The key comes only from init and is
// applied with PRAGMA hexkey on every new connection, never in the file URI.
//
// Besides whatsmeow's tables, the same encrypted file holds three small tables
// of the bridge: sent outbox ids (the last 1,000), download descriptors of
// reported media (pruned after 90 days) and the JIDs of reported chats. None of
// them holds message text.
package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum" // registers the "adiantum" VFS

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// File names inside the store folder.
const (
	DBFile   = "store.db"
	LockFile = "bridge.lock"
)

// Errors of Open.
var (
	ErrKeyInvalid = errors.New("store_key_invalid")
	ErrLocked     = errors.New("store_locked")
	ErrNoKey      = errors.New("store key missing")
)

// IOError wraps a disk error (store_io).
type IOError struct{ Err error }

func (e *IOError) Error() string { return "store_io: " + e.Err.Error() }
func (e *IOError) Unwrap() error { return e.Err }

// OutboxKeep is how many outbox ids are remembered (PROTOCOL.md §6.5).
const OutboxKeep = 1000

// MediaRetention is how long media descriptors are kept.
const MediaRetention = 90 * 24 * time.Hour

// Store is an open, locked, encrypted session store.
type Store struct {
	Dir       string
	DB        *sql.DB
	Container *sqlstore.Container
	unlock    func()
}

// Open opens (or creates) the store in dir with a 32-byte key.
func Open(ctx context.Context, dir string, key []byte, log waLog.Logger) (*Store, error) {
	if len(key) != 32 {
		return nil, ErrNoKey
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, &IOError{err}
	}
	if err := restrictMode(dir, true); err != nil {
		return nil, &IOError{err}
	}
	unlock, err := lockFile(filepath.Join(dir, LockFile))
	if err != nil {
		if errors.Is(err, ErrLocked) {
			return nil, ErrLocked
		}
		return nil, &IOError{err}
	}
	s, err := open(ctx, dir, key, log)
	if err != nil {
		unlock()
		return nil, err
	}
	s.unlock = unlock
	return s, nil
}

func dsn(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive paths: file:///C:/...
	}
	q := url.Values{}
	q.Set("vfs", "adiantum")
	u := url.URL{Scheme: "file", Path: p, RawQuery: q.Encode()}
	return u.String()
}

func open(ctx context.Context, dir string, key []byte, log waLog.Logger) (*Store, error) {
	dbPath := filepath.Join(dir, DBFile)
	if f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0o600); err != nil {
		return nil, &IOError{err}
	} else {
		f.Close()
	}
	hexKey := hex.EncodeToString(key)
	initConn := func(c *sqlite3.Conn) error {
		// The key first, before anything reads the file.
		if err := c.Exec("PRAGMA hexkey='" + hexKey + "'"); err != nil {
			return err
		}
		return c.Exec("PRAGMA foreign_keys=ON; PRAGMA temp_store=MEMORY; PRAGMA busy_timeout=10000; PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL;")
	}
	db, err := driver.Open(dsn(dbPath), initConn)
	if err != nil {
		return nil, classify(err)
	}
	db.SetMaxOpenConns(4)
	// Touch the file: a wrong key fails here with "file is not a database".
	var n int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		db.Close()
		return nil, classify(err)
	}
	container := sqlstore.NewWithDB(db, "sqlite3", log)
	if err := container.Upgrade(ctx); err != nil {
		db.Close()
		return nil, classify(err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, classify(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			_ = restrictMode(dbPath+suffix, false)
		}
	}
	return &Store{Dir: dir, DB: db, Container: container}, nil
}

func classify(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite3.Error
	if errors.As(err, &se) && se.Code() == sqlite3.NOTADB {
		return ErrKeyInvalid
	}
	if strings.Contains(err.Error(), "file is not a database") {
		return ErrKeyInvalid
	}
	return &IOError{err}
}

const schema = `
CREATE TABLE IF NOT EXISTS voolo_sent (
	outbox_id  TEXT PRIMARY KEY,
	message_id TEXT,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS voolo_media (
	chat_jid        TEXT NOT NULL,
	message_id      TEXT NOT NULL,
	kind            TEXT NOT NULL,
	mime            TEXT NOT NULL,
	size_bytes      INTEGER NOT NULL,
	duration_s      INTEGER NOT NULL DEFAULT 0,
	direct_path     TEXT NOT NULL,
	media_key       BLOB NOT NULL,
	file_sha256     BLOB,
	file_enc_sha256 BLOB,
	msg_ts          INTEGER NOT NULL,
	PRIMARY KEY (chat_jid, message_id)
);
CREATE TABLE IF NOT EXISTS voolo_chats (
	jid        TEXT PRIMARY KEY,
	lid_alias_pending INTEGER NOT NULL DEFAULT 0
);
`

// Close closes the database and releases the lock.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.DB != nil {
		// Fold the WAL back into the main file before closing.
		_, _ = s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		err = s.Container.Close()
		s.DB = nil
	}
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return err
}

// Wipe closes the store and deletes its files (the lock file stays, unlocked).
func (s *Store) Wipe() error {
	if s.DB != nil {
		_ = s.Container.Close()
		s.DB = nil
	}
	var firstErr error
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(filepath.Join(s.Dir, DBFile+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = &IOError{err}
		}
	}
	if s.unlock != nil {
		s.unlock()
		s.unlock = nil
	}
	return firstErr
}

// ReserveOutbox records an outbox id before its send. It returns dup=true,
// and records nothing, if the id is already among the remembered ones.
func (s *Store) ReserveOutbox(ctx context.Context, outboxID string, now time.Time) (dup bool, err error) {
	res, err := s.DB.ExecContext(ctx, `INSERT INTO voolo_sent (outbox_id, created_at) VALUES ($1, $2) ON CONFLICT (outbox_id) DO NOTHING`, outboxID, now.UnixMilli())
	if err != nil {
		return false, &IOError{err}
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return true, nil
	}
	_, err = s.DB.ExecContext(ctx, `DELETE FROM voolo_sent WHERE outbox_id NOT IN (SELECT outbox_id FROM voolo_sent ORDER BY created_at DESC, rowid DESC LIMIT $1)`, OutboxKeep)
	if err != nil {
		return false, &IOError{err}
	}
	return false, nil
}

// SetOutboxMessage stores the WhatsApp message id of a sent outbox id.
func (s *Store) SetOutboxMessage(ctx context.Context, outboxID, messageID string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE voolo_sent SET message_id=$1 WHERE outbox_id=$2`, messageID, outboxID)
	if err != nil {
		return &IOError{err}
	}
	return nil
}

// MediaDesc is what fetch_media needs to download a reported attachment.
type MediaDesc struct {
	ChatJID       string
	MessageID     string
	Kind          string
	Mime          string
	SizeBytes     int64
	DurationS     int
	DirectPath    string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	MsgTS         int64
}

// PutMedia upserts media descriptors in one transaction.
func (s *Store) PutMedia(ctx context.Context, ds []MediaDesc) error {
	if len(ds) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return &IOError{err}
	}
	defer tx.Rollback()
	for _, d := range ds {
		_, err := tx.ExecContext(ctx, `INSERT INTO voolo_media (chat_jid, message_id, kind, mime, size_bytes, duration_s, direct_path, media_key, file_sha256, file_enc_sha256, msg_ts)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			ON CONFLICT (chat_jid, message_id) DO UPDATE SET kind=excluded.kind, mime=excluded.mime, size_bytes=excluded.size_bytes,
			duration_s=excluded.duration_s, direct_path=excluded.direct_path, media_key=excluded.media_key,
			file_sha256=excluded.file_sha256, file_enc_sha256=excluded.file_enc_sha256, msg_ts=excluded.msg_ts`,
			d.ChatJID, d.MessageID, d.Kind, d.Mime, d.SizeBytes, d.DurationS, d.DirectPath, d.MediaKey, d.FileSHA256, d.FileEncSHA256, d.MsgTS)
		if err != nil {
			return &IOError{err}
		}
	}
	if err := tx.Commit(); err != nil {
		return &IOError{err}
	}
	return nil
}

// GetMedia returns a descriptor, or ok=false.
func (s *Store) GetMedia(ctx context.Context, chatJID, messageID string) (d MediaDesc, ok bool, err error) {
	row := s.DB.QueryRowContext(ctx, `SELECT chat_jid, message_id, kind, mime, size_bytes, duration_s, direct_path, media_key, file_sha256, file_enc_sha256, msg_ts
		FROM voolo_media WHERE chat_jid=$1 AND message_id=$2`, chatJID, messageID)
	err = row.Scan(&d.ChatJID, &d.MessageID, &d.Kind, &d.Mime, &d.SizeBytes, &d.DurationS, &d.DirectPath, &d.MediaKey, &d.FileSHA256, &d.FileEncSHA256, &d.MsgTS)
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, &IOError{err}
	}
	return d, true, nil
}

// PruneMedia deletes descriptors of messages older than the retention.
func (s *Store) PruneMedia(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `DELETE FROM voolo_media WHERE msg_ts < $1`, now.Add(-MediaRetention).UnixMilli())
	if err != nil {
		return 0, &IOError{err}
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AddChats records reported chat JIDs. lidPending marks @lid chats whose
// phone-number JID is not known yet.
func (s *Store) AddChats(ctx context.Context, jids []string) error {
	if len(jids) == 0 {
		return nil
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return &IOError{err}
	}
	defer tx.Rollback()
	for _, j := range jids {
		pending := 0
		if strings.HasSuffix(j, "@lid") {
			pending = 1
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO voolo_chats (jid, lid_alias_pending) VALUES ($1, $2) ON CONFLICT (jid) DO NOTHING`, j, pending); err != nil {
			return &IOError{err}
		}
	}
	if err := tx.Commit(); err != nil {
		return &IOError{err}
	}
	return nil
}

// Chats returns every reported chat JID and whether it is a @lid chat waiting
// for its phone-number alias.
func (s *Store) Chats(ctx context.Context) (map[string]bool, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT jid, lid_alias_pending FROM voolo_chats`)
	if err != nil {
		return nil, &IOError{err}
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var j string
		var p int
		if err := rows.Scan(&j, &p); err != nil {
			return nil, &IOError{err}
		}
		out[j] = p == 1
	}
	return out, rows.Err()
}

// ResolveLIDChat marks a reported @lid chat as aliased to pn (and records pn).
func (s *Store) ResolveLIDChat(ctx context.Context, lid, pn string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return &IOError{err}
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE voolo_chats SET lid_alias_pending=0 WHERE jid=$1`, lid); err != nil {
		return &IOError{err}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO voolo_chats (jid, lid_alias_pending) VALUES ($1, 0) ON CONFLICT (jid) DO NOTHING`, pn); err != nil {
		return &IOError{err}
	}
	if err := tx.Commit(); err != nil {
		return &IOError{err}
	}
	return nil
}

// String never reveals the path (for accidental %v use).
func (s *Store) String() string { return fmt.Sprintf("store(open=%v)", s != nil && s.DB != nil) }
