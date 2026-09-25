// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Package media implements the media hand-off of PROTOCOL.md §8: files of the
// form nonce (12 bytes) ‖ ciphertext ‖ tag (16 bytes), AES-256-GCM with a fresh
// random 32-byte key per file and no additional data. Plaintext media never
// touches the disk. It also holds the size and duration caps (ADR-012 S5, as
// amended by the owner on 2026-09-25).
package media

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Overhead is the nonce plus the GCM tag.
const Overhead = 12 + 16

// DownloadOverhead is what WhatsApp's encrypted media adds to the plaintext:
// up to 16 bytes of AES-CBC padding and a 10-byte MAC. A download of a file
// capped at n bytes is cut off after n + DownloadOverhead bytes.
const DownloadOverhead = 16 + 10

// testHookAfterCheck, when set by a test, runs between the path check and the
// read in Open.
var testHookAfterCheck func(path string)

// StaleAfter is the age after which leftover hand-off files are deleted at start.
const StaleAfter = time.Hour

var fileNameRe = regexp.MustCompile(`^[0-9a-f]{32}\.bin$`)

// Errors.
var (
	ErrInvalid = errors.New("media_invalid")
	ErrTooBig  = errors.New("media_too_large")
)

// Sealed describes a written hand-off file.
type Sealed struct {
	Path      string
	Key       string // 64 hex characters
	SHA256    string // hex SHA-256 of the plaintext
	SizeBytes int64
}

// Seal encrypts plaintext under a fresh key into <dir>/<random 32 hex>.bin.
func Seal(dir string, plaintext []byte) (Sealed, error) {
	key := make([]byte, 32)
	nonce := make([]byte, 12)
	name := make([]byte, 16)
	for _, b := range [][]byte{key, nonce, name} {
		if _, err := rand.Read(b); err != nil {
			return Sealed{}, err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Sealed{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Sealed{}, err
	}
	out := make([]byte, 0, len(plaintext)+Overhead)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plaintext, nil)
	path := filepath.Join(dir, hex.EncodeToString(name)+".bin")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Sealed{}, err
	}
	if _, err := f.Write(out); err != nil {
		f.Close()
		os.Remove(path)
		return Sealed{}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return Sealed{}, err
	}
	sum := sha256.Sum256(plaintext)
	return Sealed{Path: path, Key: hex.EncodeToString(key), SHA256: hex.EncodeToString(sum[:]), SizeBytes: int64(len(plaintext))}, nil
}

// InDir reports whether path is a hand-off file name directly inside dir.
func InDir(dir, path string) bool {
	if filepath.Clean(filepath.Dir(path)) != filepath.Clean(dir) {
		return false
	}
	return fileNameRe.MatchString(filepath.Base(path))
}

// Open reads, decrypts and checks a hand-off file written by the client for
// send_media. The file must be a regular file (not a link) directly inside
// dir; its plaintext must not exceed maxBytes and must hash to sha256Hex. The
// file is deleted in every case once it has been looked at. The file is
// opened once and every check after the name is made on that open handle, so
// a path swapped for a link, or a file truncated, after the first check is
// refused (review L4).
func Open(dir, path, keyHex, sha256Hex string, maxBytes int64) ([]byte, error) {
	if !InDir(dir, path) {
		return nil, ErrInvalid
	}
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, ErrInvalid
	}
	defer os.Remove(path)
	if testHookAfterCheck != nil {
		testHookAfterCheck(path)
	}
	f, err := openNoFollow(path)
	if err != nil {
		return nil, ErrInvalid
	}
	defer f.Close()
	hfi, err := f.Stat()
	if err != nil || !hfi.Mode().IsRegular() || !os.SameFile(fi, hfi) {
		return nil, ErrInvalid
	}
	if hfi.Size() < Overhead {
		return nil, ErrInvalid
	}
	if hfi.Size()-Overhead > maxBytes {
		return nil, ErrTooBig
	}
	key, err := hex.DecodeString(keyHex)
	if err != nil || len(key) != 32 {
		return nil, ErrInvalid
	}
	want, err := hex.DecodeString(sha256Hex)
	if err != nil || len(want) != 32 {
		return nil, ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+Overhead+1))
	if err != nil {
		return nil, ErrInvalid
	}
	if int64(len(data)) > maxBytes+Overhead {
		return nil, ErrTooBig
	}
	if len(data) < Overhead {
		return nil, ErrInvalid // truncated after the size check
	}
	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, data[:12], data[12:], nil)
	if err != nil {
		return nil, ErrInvalid
	}
	got := sha256.Sum256(plain)
	if subtle.ConstantTimeCompare(got[:], want) != 1 {
		return nil, ErrInvalid
	}
	return plain, nil
}

// Discard deletes a hand-off file named by a send_media that was refused
// before the file was read (PROTOCOL.md §6.6: deleted in every case). Only a
// hand-off name directly inside dir is touched; a link is removed, never its
// target.
func Discard(dir, path string) {
	if InDir(dir, path) {
		_ = os.Remove(path)
	}
}

// CleanStale deletes hand-off files in dir older than StaleAfter. It returns
// how many were deleted. Other files are left alone.
func CleanStale(dir string, now time.Time) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.Type().IsRegular() || !fileNameRe.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil || now.Sub(fi.ModTime()) < StaleAfter {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}

// Limits are the caps that apply to fetch_media and send_media.
type Limits struct {
	ImageMaxBytes   int64
	VoiceMaxSeconds int
	VoiceMaxBytes   int64
	FileMaxBytes    int64
}

// Fetchable reports whether a reported kind can be fetched in v1 (voice notes
// and images only; video, documents, stickers and other audio stay on the phone).
func Fetchable(kind string) bool { return kind == "image" || kind == "voice" }

// CheckFetch applies the caps to a descriptor before any download.
func CheckFetch(kind string, sizeBytes int64, durationS int, l Limits) error {
	switch kind {
	case "image":
		if sizeBytes > l.ImageMaxBytes {
			return ErrTooBig
		}
	case "voice":
		if durationS > l.VoiceMaxSeconds || sizeBytes > l.VoiceMaxBytes {
			return ErrTooBig
		}
	default:
		return fmt.Errorf("not fetchable")
	}
	return nil
}

// VoiceMime reports whether a send_media voice note's mime type is Ogg (Opus),
// the only format whose duration the bridge reads from the file.
func VoiceMime(m string) bool {
	return m == "audio/ogg" || strings.HasPrefix(m, "audio/ogg;")
}

// MaxSendBytes is the size cap for a send_media kind.
func MaxSendBytes(kind string, l Limits) int64 {
	switch kind {
	case "image":
		return l.ImageMaxBytes
	case "voice":
		return l.VoiceMaxBytes
	default:
		return l.FileMaxBytes
	}
}

// OggDurationSeconds returns the duration of an Ogg Opus stream (from the
// granule position of the last page, at 48 kHz, minus the pre-skip), rounded
// up, or 0 when it cannot tell.
func OggDurationSeconds(b []byte) int {
	if len(b) < 27 || !strings.HasPrefix(string(b[:4]), "OggS") {
		return 0
	}
	var preSkip uint16
	if i := indexOf(b, "OpusHead"); i >= 0 && i+12 <= len(b) {
		preSkip = binary.LittleEndian.Uint16(b[i+10 : i+12])
	}
	last := -1
	for i := len(b) - 27; i >= 0; i-- {
		if b[i] == 'O' && string(b[i:i+4]) == "OggS" {
			last = i
			break
		}
	}
	if last < 0 {
		return 0
	}
	granule := binary.LittleEndian.Uint64(b[last+6 : last+14])
	if granule == ^uint64(0) || granule <= uint64(preSkip) {
		return 0
	}
	samples := granule - uint64(preSkip)
	return int((samples + 47999) / 48000)
}

func indexOf(b []byte, s string) int {
	n := len(s)
	for i := 0; i+n <= len(b) && i < 4096; i++ {
		if string(b[i:i+n]) == s {
			return i
		}
	}
	return -1
}
