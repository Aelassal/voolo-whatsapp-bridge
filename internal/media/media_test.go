// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package media

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

var plaintext = bytes.Repeat([]byte("VOICE-NOTE-PLAINTEXT-"), 500)

func TestSealWritesCiphertextOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := Seal(dir, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if !InDir(dir, s.Path) {
		t.Fatalf("path %s not a hand-off file in dir", s.Path)
	}
	raw, _ := os.ReadFile(s.Path)
	if bytes.Contains(raw, []byte("VOICE-NOTE")) {
		t.Fatal("temp file contains plaintext")
	}
	if len(raw) != len(plaintext)+Overhead {
		t.Fatalf("size %d", len(raw))
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(s.Path)
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %o", fi.Mode().Perm())
		}
	}
	// Decrypt as a client would, from the documented format.
	key, _ := hex.DecodeString(s.Key)
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	got, err := gcm.Open(nil, raw[:12], raw[12:], nil)
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatal("client-side decryption failed")
	}
	sum := sha256.Sum256(plaintext)
	if s.SHA256 != hex.EncodeToString(sum[:]) || s.SizeBytes != int64(len(plaintext)) {
		t.Fatal("sha256/size describe the plaintext")
	}
}

func TestKeyNonceAndNameFreshPerFile(t *testing.T) {
	dir := t.TempDir()
	a, _ := Seal(dir, plaintext)
	b, _ := Seal(dir, plaintext)
	ra, _ := os.ReadFile(a.Path)
	rb, _ := os.ReadFile(b.Path)
	if a.Key == b.Key || a.Path == b.Path || bytes.Equal(ra[:12], rb[:12]) || bytes.Equal(ra, rb) {
		t.Fatal("key, nonce or file must differ per file")
	}
}

func TestOpenRoundTripAndDeletes(t *testing.T) {
	dir := t.TempDir()
	s, _ := Seal(dir, plaintext)
	got, err := Open(dir, s.Path, s.Key, s.SHA256, int64(len(plaintext)))
	if err != nil || !bytes.Equal(got, plaintext) {
		t.Fatalf("open: %v", err)
	}
	if _, err := os.Stat(s.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("file not deleted after use")
	}
}

func TestOpenRefusals(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	cases := []struct {
		name string
		prep func() (path, key, sha string, max int64)
		want error
	}{
		{"wrong key", func() (string, string, string, int64) {
			s, _ := Seal(dir, plaintext)
			return s.Path, hex.EncodeToString(bytes.Repeat([]byte{1}, 32)), s.SHA256, 1 << 20
		}, ErrInvalid},
		{"wrong hash", func() (string, string, string, int64) {
			s, _ := Seal(dir, plaintext)
			return s.Path, s.Key, hex.EncodeToString(make([]byte, 32)), 1 << 20
		}, ErrInvalid},
		{"too large", func() (string, string, string, int64) {
			s, _ := Seal(dir, plaintext)
			return s.Path, s.Key, s.SHA256, int64(len(plaintext) - 1)
		}, ErrTooBig},
		{"outside dir", func() (string, string, string, int64) {
			s, _ := Seal(other, plaintext)
			return s.Path, s.Key, s.SHA256, 1 << 20
		}, ErrInvalid},
		{"traversal", func() (string, string, string, int64) {
			return filepath.Join(dir, "..", "x.bin"), "", "", 1 << 20
		}, ErrInvalid},
		{"missing", func() (string, string, string, int64) {
			return filepath.Join(dir, "0123456789abcdef0123456789abcdef.bin"), "", "", 1 << 20
		}, ErrInvalid},
	}
	for _, c := range cases {
		p, k, h, m := c.prep()
		if _, err := Open(dir, p, k, h, m); !errors.Is(err, c.want) {
			t.Errorf("%s: want %v, got %v", c.name, c.want, err)
		}
	}
	if runtime.GOOS != "windows" {
		s, _ := Seal(other, plaintext)
		link := filepath.Join(dir, "0123456789abcdef0123456789abcdee.bin")
		if err := os.Symlink(s.Path, link); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, link, s.Key, s.SHA256, 1<<20); !errors.Is(err, ErrInvalid) {
			t.Fatalf("symlink accepted: %v", err)
		}
	}
}

func TestCleanStale(t *testing.T) {
	dir := t.TempDir()
	old, _ := Seal(dir, []byte("a"))
	fresh, _ := Seal(dir, []byte("b"))
	keep := filepath.Join(dir, "notes.txt")
	_ = os.WriteFile(keep, []byte("x"), 0o600)
	past := time.Now().Add(-2 * time.Hour)
	_ = os.Chtimes(old.Path, past, past)
	_ = os.Chtimes(keep, past, past)
	if n := CleanStale(dir, time.Now()); n != 1 {
		t.Fatalf("deleted %d", n)
	}
	for _, p := range []string{fresh.Path, keep} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s removed", filepath.Base(p))
		}
	}
}

// S5 caps as amended by the owner: voice ≤ 60 min and ≤ 32 MiB, images ≤ 16 MiB,
// checked before any download; boundary values on both sides.
func TestFetchCaps(t *testing.T) {
	l := Limits{ImageMaxBytes: 16 << 20, VoiceMaxSeconds: 3600, VoiceMaxBytes: 32 << 20}
	cases := []struct {
		kind string
		size int64
		dur  int
		ok   bool
	}{
		{"image", 16 << 20, 0, true},
		{"image", 16<<20 + 1, 0, false},
		{"voice", 1000, 3599, true},
		{"voice", 1000, 3600, true},
		{"voice", 1000, 3601, false},
		{"voice", 32 << 20, 60, true},
		{"voice", 32<<20 + 1, 60, false},
		{"video", 10, 1, false},
		{"document", 10, 0, false},
		{"sticker", 10, 0, false},
		{"audio", 10, 1, false},
	}
	for _, c := range cases {
		err := CheckFetch(c.kind, c.size, c.dur, l)
		if (err == nil) != c.ok || (Fetchable(c.kind) != (c.kind == "image" || c.kind == "voice")) {
			t.Errorf("%+v: err=%v", c, err)
		}
	}
}

func oggPage(granule uint64, payload []byte) []byte {
	h := make([]byte, 27)
	copy(h, "OggS")
	binary.LittleEndian.PutUint64(h[6:], granule)
	return append(h, payload...)
}

func TestOggDuration(t *testing.T) {
	head := append([]byte("OpusHead"), 1, 1, 0x38, 0x01) // pre-skip 312
	stream := append(oggPage(0, head), oggPage(48000*14+312, []byte("data"))...)
	if d := OggDurationSeconds(stream); d != 14 {
		t.Fatalf("duration %d", d)
	}
	if OggDurationSeconds([]byte("not ogg at all, not ogg at all")) != 0 {
		t.Fatal("non-ogg")
	}
}

// Review L4: the file checked is the file read. A path swapped for a link to
// another valid hand-off file after the check is refused, and a file truncated
// after the check gives media_invalid, not a panic.
func TestOpenChecksTheHandleItReads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	target, _ := Seal(dir, plaintext)
	path := filepath.Join(dir, "0123456789abcdef0123456789abcde0.bin")
	if err := os.WriteFile(path, bytes.Repeat([]byte{7}, 200), 0o600); err != nil {
		t.Fatal(err)
	}
	testHookAfterCheck = func(p string) {
		_ = os.Remove(p)
		_ = os.Symlink(target.Path, p)
	}
	defer func() { testHookAfterCheck = nil }()
	if _, err := Open(dir, path, target.Key, target.SHA256, 1<<20); !errors.Is(err, ErrInvalid) {
		t.Fatalf("swapped link: want ErrInvalid, got %v", err)
	}

	s, _ := Seal(dir, plaintext)
	testHookAfterCheck = func(p string) { _ = os.Truncate(p, 5) }
	if _, err := Open(dir, s.Path, s.Key, s.SHA256, 1<<20); !errors.Is(err, ErrInvalid) {
		t.Fatalf("truncated: want ErrInvalid, got %v", err)
	}
	if _, err := os.Stat(s.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("truncated file not deleted")
	}
}

// Review L4: Discard deletes a hand-off file named in a refused send_media,
// and nothing outside the media folder.
func TestDiscard(t *testing.T) {
	dir, other := t.TempDir(), t.TempDir()
	in, _ := Seal(dir, plaintext)
	out, _ := Seal(other, plaintext)
	Discard(dir, in.Path)
	Discard(dir, out.Path)
	if _, err := os.Stat(in.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("hand-off file kept")
	}
	if _, err := os.Stat(out.Path); err != nil {
		t.Fatal("file outside the media folder deleted")
	}
}
