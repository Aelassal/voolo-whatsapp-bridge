// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	"go.mau.fi/whatsmeow/store/sqlstore/upgrades"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

func TestMain(m *testing.M) {
	RestrictUmask()
	os.Exit(m.Run())
}

const marker = "VOOLO_PLAINTEXT_MARKER_7f3a"

func TestNoPlaintextInDBWALOrSHM(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(ctx, dir, key(7), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Keep data in the WAL so the -wal file is checked too.
	if _, err := s.DB.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("CREATE TABLE t (v TEXT)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		if _, err := s.DB.Exec("INSERT INTO t (v) VALUES ($1)", strings.Repeat(marker, 20)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddChats(ctx, []string{"15550100002@s.whatsapp.net"}); err != nil {
		t.Fatal(err)
	}
	check := func(stage string) {
		found := 0
		for _, suffix := range []string{"", "-wal", "-shm"} {
			b, err := os.ReadFile(filepath.Join(dir, DBFile+suffix))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			found++
			for _, needle := range []string{marker, "SQLite format 3", "15550100002", "voolo_sent", "whatsmeow_device"} {
				if bytes.Contains(b, []byte(needle)) {
					t.Fatalf("%s: %s%s contains plaintext %q", stage, DBFile, suffix, needle)
				}
			}
		}
		if found < 2 {
			t.Fatalf("%s: expected the db and its WAL to exist, found %d files", stage, found)
		}
	}
	check("open")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, DBFile))
	if len(b) == 0 || bytes.Contains(b, []byte(marker)) {
		t.Fatal("after checkpoint the main file must hold the data, encrypted")
	}
}

func TestWrongKeyFailsAndPlainOpenFails(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir, key(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, _ := os.ReadFile(filepath.Join(dir, DBFile))

	if _, err := Open(ctx, dir, key(2), nil); !errors.Is(err, ErrKeyInvalid) {
		t.Fatalf("wrong key: want ErrKeyInvalid, got %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, DBFile))
	if !bytes.Equal(before, after) {
		t.Fatal("a wrong key must never modify or recreate the store")
	}
	plain, err := driver.Open("file:" + filepath.ToSlash(filepath.Join(dir, DBFile)) + "?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	var n int
	if err := plain.QueryRow("SELECT count(*) FROM sqlite_master").Scan(&n); err == nil {
		t.Fatal("the store opened without a key")
	}
	// The right key still works (the lock was released by the failed open).
	s, err = Open(ctx, dir, key(1), nil)
	if err != nil {
		t.Fatalf("right key after a wrong one: %v", err)
	}
	s.Close()
}

func TestNoKeyRefused(t *testing.T) {
	for _, k := range [][]byte{nil, {}, key(1)[:31], append(key(1), 1)} {
		if _, err := Open(context.Background(), t.TempDir(), k, nil); !errors.Is(err, ErrNoKey) {
			t.Fatalf("len %d: want ErrNoKey, got %v", len(k), err)
		}
	}
}

func TestSecondOpenIsLocked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir, key(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(ctx, dir, key(1), nil); !errors.Is(err, ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
	s.Close()
	s2, err := Open(ctx, dir, key(1), nil)
	if err != nil {
		t.Fatalf("after close: %v", err)
	}
	s2.Close()
}

func TestSchemaIsCurrent(t *testing.T) {
	s, err := Open(context.Background(), t.TempDir(), key(3), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.DB.QueryRow("SELECT version FROM whatsmeow_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != len(upgrades.Table) || v < 15 {
		t.Fatalf("whatsmeow_version = %d, want %d", v, len(upgrades.Table))
	}
	var mode string
	_ = s.DB.QueryRow("PRAGMA journal_mode").Scan(&mode)
	var fk int
	_ = s.DB.QueryRow("PRAGMA foreign_keys").Scan(&fk)
	var ts int
	_ = s.DB.QueryRow("PRAGMA temp_store").Scan(&ts)
	if mode != "wal" || fk != 1 || ts != 2 {
		t.Fatalf("pragmas: journal=%s fk=%d temp_store=%d", mode, fk, ts)
	}
}

func TestFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses the profile ACL")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "store")
	s, err := Open(context.Background(), dir, key(4), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.DB.Exec("CREATE TABLE x (a)")
	defer s.Close()
	fi, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) < 3 {
		t.Fatalf("expected db, wal/shm and lock, got %d entries", len(entries))
	}
	for _, e := range entries {
		fi, _ := e.Info()
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %o", e.Name(), fi.Mode().Perm())
		}
	}
	// A pre-existing folder with wide permissions is tightened.
	wide := filepath.Join(parent, "wide")
	_ = os.Mkdir(wide, 0o755)
	_ = os.Chmod(wide, 0o755)
	s2, err := Open(context.Background(), wide, key(4), nil)
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	fi, _ = os.Stat(wide)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("existing dir not tightened: %o", fi.Mode().Perm())
	}
}

func TestOutboxRememberedAcrossRestartAndCapped(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir, key(5), nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.UnixMilli(1790330400000)
	dup, err := s.ReserveOutbox(ctx, "01M3C03V80N87VFZS5G0J0NFEX", now)
	if err != nil || dup {
		t.Fatalf("first reserve: dup=%v err=%v", dup, err)
	}
	if dup, _ := s.ReserveOutbox(ctx, "01M3C03V80N87VFZS5G0J0NFEX", now); !dup {
		t.Fatal("repeat not detected")
	}
	_ = s.SetOutboxMessage(ctx, "01M3C03V80N87VFZS5G0J0NFEX", "3EB0")
	s.Close()
	s, err = Open(ctx, dir, key(5), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if dup, _ := s.ReserveOutbox(ctx, "01M3C03V80N87VFZS5G0J0NFEX", now); !dup {
		t.Fatal("repeat not detected after restart")
	}
	for i := 0; i < OutboxKeep+5; i++ {
		id := "X" + time.Duration(i).String()
		if _, err := s.ReserveOutbox(ctx, id, now.Add(time.Duration(i+1)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = s.DB.QueryRow("SELECT count(*) FROM voolo_sent").Scan(&n)
	if n != OutboxKeep {
		t.Fatalf("kept %d outbox ids, want %d", n, OutboxKeep)
	}
}

func TestMediaDescriptorsAndPrune(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir(), key(6), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.UnixMilli(1790330400000)
	old := MediaDesc{ChatJID: "a@s.whatsapp.net", MessageID: "OLD", Kind: "voice", Mime: "audio/ogg", SizeBytes: 10, DirectPath: "/v/t62", MediaKey: []byte{1}, MsgTS: now.Add(-91 * 24 * time.Hour).UnixMilli()}
	fresh := old
	fresh.MessageID, fresh.MsgTS = "NEW", now.UnixMilli()
	if err := s.PutMedia(ctx, []MediaDesc{old, fresh}); err != nil {
		t.Fatal(err)
	}
	if d, ok, err := s.GetMedia(ctx, "a@s.whatsapp.net", "NEW"); err != nil || !ok || d.DirectPath != "/v/t62" {
		t.Fatalf("get: %v %v %+v", ok, err, d)
	}
	if n, _ := s.PruneMedia(ctx, now); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	if _, ok, _ := s.GetMedia(ctx, "a@s.whatsapp.net", "OLD"); ok {
		t.Fatal("old descriptor kept")
	}
}

// A message reported under a @lid chat is still found after the client merged
// that chat into its phone-number JID (chat_update aliasOf).
func TestGetMediaFallsBackToMessageIDAfterAlias(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir(), key(11), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := MediaDesc{ChatJID: "100000000000002@lid", MessageID: "3EB0V", Kind: "voice", Mime: "audio/ogg", SizeBytes: 1, DirectPath: "/v", MediaKey: []byte{1}, MsgTS: 1}
	_ = s.PutMedia(ctx, []MediaDesc{d})
	if got, ok, _ := s.GetMedia(ctx, "15550100002@s.whatsapp.net", "3EB0V"); !ok || got.ChatJID != d.ChatJID {
		t.Fatal("not found under the phone-number JID")
	}
	// Ambiguous ids (same id in two chats) are not guessed.
	d2 := d
	d2.ChatJID = "15550100009@s.whatsapp.net"
	_ = s.PutMedia(ctx, []MediaDesc{d2})
	if _, ok, _ := s.GetMedia(ctx, "15550100002@s.whatsapp.net", "3EB0V"); ok {
		t.Fatal("guessed between two chats")
	}
}

func TestWipeDeletesStoreFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(ctx, dir, key(8), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.DB.Exec("CREATE TABLE x (a)")
	if err := s.Wipe(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, DBFile+suffix)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s%s still exists", DBFile, suffix)
		}
	}
	// A fresh store can be created in the same folder, with a new key.
	s, err = Open(ctx, dir, key(9), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
}

func TestChatsAndLIDResolution(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, t.TempDir(), key(10), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_ = s.AddChats(ctx, []string{"100000000000001@lid", "15550100002@s.whatsapp.net"})
	m, _ := s.Chats(ctx)
	if !m["100000000000001@lid"] || m["15550100002@s.whatsapp.net"] {
		t.Fatalf("pending flags wrong: %v", m)
	}
	_ = s.ResolveLIDChat(ctx, "100000000000001@lid", "15550100009@s.whatsapp.net")
	m, _ = s.Chats(ctx)
	if m["100000000000001@lid"] {
		t.Fatal("still pending")
	}
	if _, ok := m["15550100009@s.whatsapp.net"]; !ok {
		t.Fatal("pn not recorded")
	}
}

var _ = sql.ErrNoRows

// Review L6: the outbox reservation survives a power loss (synchronous=FULL on
// every connection, WAL mode).
func TestSynchronousFull(t *testing.T) {
	s, err := Open(context.Background(), t.TempDir(), key(12), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	conns := make([]*sql.Conn, 3) // several pooled connections, each set up by the hook
	for i := range conns {
		c, err := s.DB.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
		var sync int
		if err := c.QueryRowContext(context.Background(), "PRAGMA synchronous").Scan(&sync); err != nil || sync != 2 {
			t.Fatalf("connection %d: synchronous=%d err=%v, want 2 (FULL)", i, sync, err)
		}
	}
	for _, c := range conns {
		c.Close()
	}
}

// Review H1: every store method is safe after Close and Wipe, including while
// other goroutines are using the store: they get ErrClosed, never a panic.
func TestMethodsAfterCloseAndWipe(t *testing.T) {
	ctx := context.Background()
	for _, end := range []string{"close", "wipe"} {
		s, err := Open(ctx, t.TempDir(), key(13), nil)
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.AddChats(ctx, []string{"15550100002@s.whatsapp.net"})
				_, _, _ = s.GetMedia(ctx, "15550100002@s.whatsapp.net", "X")
			}
		}()
		time.Sleep(20 * time.Millisecond)
		if end == "close" {
			err = s.Close()
		} else {
			err = s.Wipe()
		}
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		close(stop)
		<-done
		if !s.Closed() {
			t.Fatal("Closed() false")
		}
		checks := map[string]error{}
		_, checks["ReserveOutbox"] = s.ReserveOutbox(ctx, "01M3C03V80N87VFZS5G0J0NFEX", time.Now())
		checks["SetOutboxMessage"] = s.SetOutboxMessage(ctx, "01M3C03V80N87VFZS5G0J0NFEX", "3EB0")
		checks["PutMedia"] = s.PutMedia(ctx, []MediaDesc{{ChatJID: "a", MessageID: "b"}})
		_, _, checks["GetMedia"] = s.GetMedia(ctx, "a", "b")
		_, checks["PruneMedia"] = s.PruneMedia(ctx, time.Now())
		checks["AddChats"] = s.AddChats(ctx, []string{"a"})
		_, checks["Chats"] = s.Chats(ctx)
		checks["ResolveLIDChat"] = s.ResolveLIDChat(ctx, "a", "b")
		_, checks["RecentSends"] = s.RecentSends(ctx, time.Now().Add(-time.Hour))
		_, checks["OutboxKnown"] = s.OutboxKnown(ctx, "01M3C03V80N87VFZS5G0J0NFEX")
		for name, err := range checks {
			if !errors.Is(err, ErrClosed) {
				t.Errorf("%s after %s: %v", name, end, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Errorf("second Close after %s: %v", end, err)
		}
	}
}
