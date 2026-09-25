// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	id := KeyID(pub)
	PublicKeys[id] = base64.StdEncoding.EncodeToString(pub)
	t.Cleanup(func() { delete(PublicKeys, id) })
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	return priv, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestPublishedKeyIDMatchesKey(t *testing.T) {
	for id, b64 := range PublicKeys {
		pub, _ := base64.StdEncoding.DecodeString(b64)
		if len(pub) != ed25519.PublicKeySize || KeyID(pub) != id {
			t.Fatalf("keyId %s does not match its key", id)
		}
	}
}

func TestSignExactBytesAndVerify(t *testing.T) {
	priv, _ := testKey(t)
	out, err := Sign(priv, 1790330400000, true, "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	_ = json.Unmarshal(out, &env)
	payload, _ := base64.StdEncoding.DecodeString(env.Payload)
	want := `{"v":1,"issuedAt":1790330400000,"deepSync":{"enabled":true,"maxBridgeVersion":"0.1.0"}}`
	if string(payload) != want {
		t.Fatalf("payload %s", payload)
	}
	sig, _ := base64.StdEncoding.DecodeString(env.Sig)
	if !ed25519.Verify(priv.Public().(ed25519.PublicKey), payload, sig) {
		t.Fatal("signature over the exact bytes")
	}
	if err := Verify(out); err != nil {
		t.Fatal(err)
	}
	// Tampering with the payload breaks the signature.
	env.Payload = base64.StdEncoding.EncodeToString(bytes.Replace(payload, []byte("true"), []byte("false"), 1))
	bad, _ := json.Marshal(env)
	if Verify(bad) == nil {
		t.Fatal("tampered payload verified")
	}
}

func TestSignRefusals(t *testing.T) {
	_, other, _ := ed25519.GenerateKey(rand.Reader) // not published
	if _, err := Sign(other, 1, true, "0.1.0"); err == nil {
		t.Fatal("unpublished key signed")
	}
	priv, _ := testKey(t)
	for _, v := range []string{"", "1", "1.2", "v1.2.3", "1.2.3-beta", "01.2.3"} {
		if _, err := Sign(priv, 1, true, v); err == nil {
			t.Errorf("version %q accepted", v)
		}
	}
}

func TestRunWithEnvKey(t *testing.T) {
	_, pemKey := testKey(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "flags.json")
	var so, se bytes.Buffer
	env := func(k string) string {
		if k == "FLAGS_ED25519_PRIVATE_KEY" {
			return pemKey
		}
		return ""
	}
	if c := run([]string{"-enabled=false", "-max-bridge-version=1.0.0", "-out", out}, &so, &se, env); c != 0 {
		t.Fatalf("exit %d: %s", c, se.String())
	}
	if c := run([]string{"-verify", out}, &so, &se, env); c != 0 {
		t.Fatal("verify failed")
	}
	data, _ := os.ReadFile(out)
	if strings.Contains(string(data), "PRIVATE") || strings.Contains(se.String()+so.String(), "PRIVATE") {
		t.Fatal("key material in output")
	}
	if c := run([]string{"-max-bridge-version=1.0.0"}, &so, &se, func(string) string { return "" }); c == 0 {
		t.Fatal("ran without a key")
	}
}
