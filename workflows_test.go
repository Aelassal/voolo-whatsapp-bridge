// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package voolowhatsappbridge

import (
	"os"
	"strings"
	"testing"
)

// jobs splits a workflow file into its jobs (name → the job's lines), by
// indentation: jobs are the two-space keys under "jobs:".
func jobs(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	in, cur := false, ""
	for _, l := range strings.Split(string(b), "\n") {
		switch {
		case l == "jobs:":
			in = true
		case in && len(l) > 2 && l[:2] == "  " && l[2] != ' ' && l[2] != '#' && strings.HasSuffix(l, ":"):
			cur = strings.TrimSuffix(strings.TrimSpace(l), ":")
		case in && cur != "":
			out[cur] += l + "\n"
		}
	}
	return out
}

// Review M5: only the attest job can mint an OIDC token, and it runs no
// third-party tool, no test and no build.
func TestReleaseTokenOnlyInAttestJob(t *testing.T) {
	js := jobs(t, ".github/workflows/release.yml")
	if len(js) < 5 {
		t.Fatalf("jobs %v", js)
	}
	for name, body := range js {
		hasToken := strings.Contains(body, "id-token: write") || strings.Contains(body, "attestations: write")
		if hasToken != (name == "attest") {
			t.Errorf("job %s: id-token/attestations write = %v", name, hasToken)
		}
	}
	a := js["attest"]
	for _, banned := range []string{"go install", "go test", "go-licenses", "build-release", "run:"} {
		if strings.Contains(a, banned) {
			t.Errorf("attest job runs %q", banned)
		}
	}
	if !strings.Contains(a, "subject-path: dist/voolo-whatsapp-bridge-*") {
		t.Error("attest subject does not match the release asset names")
	}
	whole, _ := os.ReadFile(".github/workflows/release.yml")
	for _, l := range strings.Split(strings.SplitN(string(whole), "\njobs:", 2)[0], "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") && strings.Contains(l, "id-token") {
			t.Error("workflow-level id-token permission")
		}
	}
}

// Review M5: the flags job runs only from main, checked by the job condition
// and again by a step before the key is used.
func TestFlagsOnlyFromMain(t *testing.T) {
	js := jobs(t, ".github/workflows/flags.yml")
	p, ok := js["publish"]
	if !ok || len(js) != 1 {
		t.Fatalf("jobs %v", js)
	}
	if !strings.Contains(p, "    if: github.ref == 'refs/heads/main'") {
		t.Error("publish job has no main-only condition")
	}
	guard := strings.Index(p, `test "$REF" = "refs/heads/main"`)
	key := strings.Index(p, "FLAGS_ED25519_PRIVATE_KEY")
	if guard < 0 || key < 0 || guard > key {
		t.Error("no main-only step before the key is used")
	}
	if strings.Contains(p, "id-token") {
		t.Error("flags job can mint an OIDC token")
	}
}

// Lead decision (2): release asset names are exactly
// voolo-whatsapp-bridge-<goos>-<goarch>, plus .exe on Windows.
func TestReleaseAssetNames(t *testing.T) {
	b, err := os.ReadFile("scripts/build-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `name="voolo-whatsapp-bridge-${goos}-${goarch}${ext}"`) || !strings.Contains(s, `[ "$goos" = windows ] && ext=".exe"`) {
		t.Fatal("asset names changed")
	}
}

// Review L8: the licence exception is pinned to the reviewed version and
// LICENSE file, and CI runs that check before the licence tool.
func TestLicenceExceptionPinned(t *testing.T) {
	b, err := os.ReadFile("scripts/check-licence-exception.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `reviewed_version="v6.3.35304"`) || !strings.Contains(string(b), "13219037ddf63dbbcf174bf59525d602df7a2e30083f63be566715c858fcb19e") {
		t.Fatal("reviewed version or hash missing")
	}
	for _, wf := range []string{".github/workflows/ci.yml", ".github/workflows/release.yml"} {
		w, _ := os.ReadFile(wf)
		s := string(w)
		pin, tool := strings.Index(s, "scripts/check-licence-exception.sh"), strings.Index(s, "go install github.com/google/go-licenses")
		if pin < 0 || tool < 0 || pin > tool {
			t.Errorf("%s: exception pin missing or after the licence tool", wf)
		}
	}
}
