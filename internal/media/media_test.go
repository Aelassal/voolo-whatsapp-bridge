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
	l := Limits{ImageMaxBytes: 16 << 20, VoiceMaxSeconds: 3600, VoiceMaxBytes: 32 << 20, VideoMaxBytes: 64 << 20, FileMaxBytes: 100 << 20}
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
		// Revision 2 (fetch_all_media): videos, stickers, documents and audio.
		{"video", 64 << 20, 1, true},
		{"video", 64<<20 + 1, 1, false},
		{"document", 100 << 20, 0, true},
		{"document", 100<<20 + 1, 0, false},
		{"audio", 100 << 20, 7200, true}, // audio files have no duration cap, only size
		{"sticker", 16 << 20, 0, true},
		{"sticker", 16<<20 + 1, 0, false},
		{"image", -1, 0, false},
		{"location", 10, 0, false},
		{"unsupported", 10, 0, false},
	}
	for _, c := range cases {
		err := CheckFetch(c.kind, c.size, c.dur, l)
		if (err == nil) != c.ok || (Fetchable(c.kind) != (c.kind != "location" && c.kind != "unsupported")) {
			t.Errorf("%+v: err=%v", c, err)
		}
	}
}

// oggPage builds one Ogg page (RFC 3533) with a correct segment table.
func oggPage(flags byte, granule uint64, serial, seq uint32, body []byte) []byte {
	var lacing []byte
	n := len(body)
	for n >= 255 {
		lacing = append(lacing, 255)
		n -= 255
	}
	lacing = append(lacing, byte(n))
	h := make([]byte, 27, 27+len(lacing)+len(body))
	copy(h, "OggS")
	h[5] = flags
	binary.LittleEndian.PutUint64(h[6:], granule)
	binary.LittleEndian.PutUint32(h[14:], serial)
	binary.LittleEndian.PutUint32(h[18:], seq)
	h[26] = byte(len(lacing))
	h = append(h, lacing...)
	return append(h, body...)
}

const serial = 0x566f6f6c

// opusHead is a 19-byte OpusHead packet with the given pre-skip.
func opusHead(preSkip uint16) []byte {
	b := append([]byte("OpusHead"), 1, 1, 0, 0, 0x80, 0xbb, 0, 0, 0, 0, 0)
	binary.LittleEndian.PutUint16(b[10:], preSkip)
	return b
}

// oggOpus is a stream of an OpusHead page, an OpusTags page and one audio
// page per granule.
func oggOpus(preSkip uint16, granules ...uint64) []byte {
	b := oggPage(0x02, 0, serial, 0, opusHead(preSkip))
	b = append(b, oggPage(0, 0, serial, 1, []byte("OpusTags\x00\x00\x00\x00\x00\x00\x00\x00"))...)
	for i, g := range granules {
		flags := byte(0)
		if i == len(granules)-1 {
			flags = 0x04
		}
		b = append(b, oggPage(flags, g, serial, uint32(i+2), []byte("opus audio packet"))...)
	}
	return b
}

type pageArgs struct {
	flags   byte
	granule uint64
	serial  uint32
	seq     uint32
	body    []byte
}

func page(flags byte, granule uint64, serial, seq uint32, body []byte) pageArgs {
	return pageArgs{flags, granule, serial, seq, body}
}

func oggPages(ps ...pageArgs) []byte {
	var b []byte
	for _, p := range ps {
		b = append(b, oggPage(p.flags, p.granule, p.serial, p.seq, p.body)...)
	}
	return b
}

func TestOggDuration(t *testing.T) {
	if d := OggDurationSeconds(oggOpus(312, 48000*5, 48000*14+312)); d != 14 {
		t.Fatalf("duration %d", d)
	}
	// A page on which no packet ends has granule -1; it is skipped.
	if d := OggDurationSeconds(oggOpus(0, 48000*3, ^uint64(0), 48000*7)); d != 7 {
		t.Fatalf("duration with a -1 granule %d", d)
	}
	if OggDurationSeconds([]byte("not ogg at all, not ogg at all")) != 0 {
		t.Fatal("non-ogg")
	}
}

// Re-review R-L2: the duration is read from a walk over every page, so a
// forged page anywhere cannot shorten it, and it is never below what the
// file's size allows at Opus's highest bitrate. Anything inconsistent is
// unreadable (0).
func TestOggDurationForged(t *testing.T) {
	long := oggOpus(0, 48000*3600, 48000*7200) // a 2-hour stream
	if d := OggDurationSeconds(long); d != 7200 {
		t.Fatalf("real stream: %d", d)
	}
	// The reviewer's case: a 27-byte page with a small granule appended.
	forged := append(append([]byte(nil), long...), oggPage(0x04, 48000*3, serial, 99, nil)[:27]...)
	forged[len(forged)-1] = 0 // no segments
	if d := OggDurationSeconds(forged); d != 0 && d < 7200 {
		t.Fatalf("forged trailing page read as %d s", d)
	}
	cases := map[string][]byte{
		"trailing page, smaller granule":  append(append([]byte(nil), long...), oggPage(0x04, 48000*3, serial, 99, []byte("x"))...),
		"trailing page, other stream":     append(append([]byte(nil), long...), oggPage(0x04, 48000*3, serial+1, 0, []byte("x"))...),
		"trailing bytes after the last":   append(append([]byte(nil), long...), []byte("OggS\x00 garbage that is not a page at all.....")...),
		"last page cut short":             long[:len(long)-3],
		"granules going back":             oggOpus(0, 48000*7200, 48000*3),
		"no OpusHead":                     append(oggPage(0x02, 0, serial, 0, []byte("OpusTagsxxxxxxxxxxxxxxxx")), oggPage(0x04, 48000*3, serial, 1, []byte("x"))...),
		"short OpusHead":                  append(oggPage(0x02, 0, serial, 0, []byte("OpusHead\x01\x01\x00\x00")), oggPage(0x04, 48000*3, serial, 1, []byte("x"))...),
		"version 1":                       func() []byte { b := oggOpus(0, 48000*3); b[4] = 1; return b }(),
		"granule only up to the pre-skip": oggOpus(4000, 3000),
		"granule of -1 on every page":     oggOpus(0, ^uint64(0)),
		// Each of these only makes the stream longer, but is inconsistent all the same.
		"page after the end of stream":         append(oggOpus(0, 48000*5), oggPage(0, 48000*6, serial, 3, []byte("x"))...),
		"first page not a beginning of stream": oggPages(page(0, 0, serial, 0, opusHead(0)), page(0x04, 48000*6, serial, 1, []byte("x"))),
		"later page of another stream":         oggPages(page(0x02, 0, serial, 0, opusHead(0)), page(0, 48000*3, serial, 1, []byte("x")), page(0, 48000*6, serial+1, 2, []byte("x"))),
		"page sequence gap":                    oggPages(page(0x02, 0, serial, 0, opusHead(0)), page(0x04, 48000*6, serial, 2, []byte("x"))),
		"second beginning of stream":           oggPages(page(0x02, 0, serial, 0, opusHead(0)), page(0x02, 48000*6, serial, 1, []byte("x"))),
	}
	for name, b := range cases {
		if d := OggDurationSeconds(b); d != 0 {
			t.Errorf("%s: read as %d s, want unreadable", name, d)
		}
	}
	// Granules scaled down on every page: the size of the file still bounds
	// the duration from below.
	big := oggPage(0x02, 0, serial, 0, opusHead(0))
	body := bytes.Repeat([]byte{0x5a}, 255*255-1) // the most one page holds
	for i := 0; i < 16; i++ {                     // about 1 MB of audio
		big = append(big, oggPage(0, uint64(i+1)*3000, serial, uint32(i+1), body)...)
	}
	d := OggDurationSeconds(big)
	if floor := len(big) / MaxOggBytesPerSecond; d < floor || d < 14 {
		t.Fatalf("1 MB stream whose granules claim 1 s read as %d s, want at least %d", d, floor)
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

// Re-review R-L6 (R-mut-L4c): the checked file moved aside and a link to it
// put in its place. The handle would then be the checked file, so only
// O_NOFOLLOW refuses it.
func TestOpenRefusesLinkToTheCheckedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := t.TempDir()
	s, _ := Seal(dir, plaintext)
	aside := filepath.Join(t.TempDir(), "aside.bin")
	testHookAfterCheck = func(p string) {
		_ = os.Rename(p, aside)
		_ = os.Symlink(aside, p)
	}
	defer func() { testHookAfterCheck = nil }()
	if _, err := Open(dir, s.Path, s.Key, s.SHA256, 1<<20); !errors.Is(err, ErrInvalid) {
		t.Fatalf("link to the checked file: want ErrInvalid, got %v", err)
	}
}

// Re-review R-L6 (R-mut-L4b): a regular file renamed over the path after the
// check is not the checked file, even with the same bytes; only the
// comparison of the open handle with the checked file refuses it.
func TestOpenRefusesRenameSwap(t *testing.T) {
	dir := t.TempDir()
	s, _ := Seal(dir, plaintext)
	raw, _ := os.ReadFile(s.Path)
	other := filepath.Join(dir, "other.tmp")
	if err := os.WriteFile(other, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	testHookAfterCheck = func(p string) { _ = os.Rename(other, p) }
	defer func() { testHookAfterCheck = nil }()
	if _, err := Open(dir, s.Path, s.Key, s.SHA256, 1<<20); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rename swap: want ErrInvalid, got %v", err)
	}
}

// Re-review R-L6 (R-mut-L4d): a file cut short after the size check on the
// handle, before it is read, gives media_invalid, not a panic.
func TestOpenFileCutShortAfterStat(t *testing.T) {
	dir := t.TempDir()
	s, _ := Seal(dir, plaintext)
	testHookAfterStat = func(p string) { _ = os.Truncate(p, 5) }
	defer func() { testHookAfterStat = nil }()
	if _, err := Open(dir, s.Path, s.Key, s.SHA256, 1<<20); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cut short after stat: want ErrInvalid, got %v", err)
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
