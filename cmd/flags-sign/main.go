// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

// Command flags-sign writes the signed flags.json of PROTOCOL.md §11. It is
// run only by the repository's "flags" workflow, in the protected "flags"
// environment, with the private key in FLAGS_ED25519_PRIVATE_KEY (PKCS#8 PEM).
// The bridge itself never reads flags.json.
//
//	flags-sign -enabled=true -max-bridge-version=0.1.0 -out flags.json
//	flags-sign -verify flags.json
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"
)

// PublicKeys are the published flags keys (PROTOCOL.md §11), by keyId.
var PublicKeys = map[string]string{
	"6856d52a7f7cded1": "Ei79aPDOwNIFwhQ0MYw2feU2LDx5rlnZiGobiuYKjj4=",
}

// KeyID is the first 16 hex characters of SHA-256 over the raw public key.
func KeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

var semverRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// Envelope is the published file.
type Envelope struct {
	KeyID   string `json:"keyId"`
	Payload string `json:"payload"`
	Sig     string `json:"sig"`
}

// PayloadBytes builds the exact payload bytes that are signed.
func PayloadBytes(issuedAt int64, enabled bool, maxBridgeVersion string) ([]byte, error) {
	if !semverRe.MatchString(maxBridgeVersion) {
		return nil, errors.New("maxBridgeVersion must be MAJOR.MINOR.PATCH")
	}
	if issuedAt <= 0 {
		return nil, errors.New("issuedAt must be positive")
	}
	return []byte(`{"v":1,"issuedAt":` + strconv.FormatInt(issuedAt, 10) + `,"deepSync":{"enabled":` + strconv.FormatBool(enabled) +
		`,"maxBridgeVersion":"` + maxBridgeVersion + `"}}`), nil
}

// Sign returns the flags.json bytes, signed by priv, whose public key must be
// one of PublicKeys (so a wrong secret can never publish an unverifiable file).
func Sign(priv ed25519.PrivateKey, issuedAt int64, enabled bool, maxBridgeVersion string) ([]byte, error) {
	pub := priv.Public().(ed25519.PublicKey)
	id := KeyID(pub)
	want, ok := PublicKeys[id]
	if !ok || want != base64.StdEncoding.EncodeToString(pub) {
		return nil, fmt.Errorf("the private key does not match a published public key (keyId %s)", id)
	}
	payload, err := PayloadBytes(issuedAt, enabled, maxBridgeVersion)
	if err != nil {
		return nil, err
	}
	env := Envelope{KeyID: id, Payload: base64.StdEncoding.EncodeToString(payload), Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, err
	}
	if err := Verify(out); err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Payload is the decoded, verified content.
type Payload struct {
	V        int   `json:"v"`
	IssuedAt int64 `json:"issuedAt"`
	DeepSync struct {
		Enabled          bool   `json:"enabled"`
		MaxBridgeVersion string `json:"maxBridgeVersion"`
	} `json:"deepSync"`
}

// Verify checks a flags.json against PublicKeys, as a client would.
func Verify(data []byte) error {
	var env Envelope
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return fmt.Errorf("envelope: %w", err)
	}
	pubB64, ok := PublicKeys[env.KeyID]
	if !ok {
		return errors.New("unknown keyId")
	}
	pub, _ := base64.StdEncoding.DecodeString(pubB64)
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return errors.New("payload is not base64")
	}
	sig, err := base64.StdEncoding.DecodeString(env.Sig)
	if err != nil || !ed25519.Verify(pub, payload, sig) {
		return errors.New("bad signature")
	}
	var p Payload
	pd := json.NewDecoder(bytes.NewReader(payload))
	pd.DisallowUnknownFields()
	if err := pd.Decode(&p); err != nil || p.V != 1 || p.IssuedAt <= 0 || !semverRe.MatchString(p.DeepSync.MaxBridgeVersion) {
		return errors.New("invalid payload")
	}
	return nil
}

func loadKey(pemData []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return priv, nil
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Getenv)) }

func run(args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("flags-sign", flag.ContinueOnError)
	fs.SetOutput(stderr)
	enabled := fs.Bool("enabled", false, "deepSync.enabled")
	maxVer := fs.String("max-bridge-version", "", "deepSync.maxBridgeVersion (MAJOR.MINOR.PATCH)")
	out := fs.String("out", "flags.json", "output file")
	verify := fs.String("verify", "", "verify this flags.json and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *verify != "" {
		data, err := os.ReadFile(*verify)
		if err == nil {
			err = Verify(data)
		}
		if err != nil {
			fmt.Fprintln(stderr, "flags-sign: verify:", err)
			return 1
		}
		fmt.Fprintln(stdout, "ok")
		return 0
	}
	pemData := getenv("FLAGS_ED25519_PRIVATE_KEY")
	if pemData == "" {
		fmt.Fprintln(stderr, "flags-sign: FLAGS_ED25519_PRIVATE_KEY is not set")
		return 1
	}
	priv, err := loadKey([]byte(pemData))
	if err != nil {
		fmt.Fprintln(stderr, "flags-sign: key:", err)
		return 1
	}
	data, err := Sign(priv, time.Now().UnixMilli(), *enabled, *maxVer)
	if err != nil {
		fmt.Fprintln(stderr, "flags-sign:", err)
		return 1
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintln(stderr, "flags-sign:", err)
		return 1
	}
	fmt.Fprintln(stdout, "wrote", *out)
	return 0
}
