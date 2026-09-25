// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package main

import (
	"bufio"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Review L5 (mutation mut-11): the running program's umask is 077, whatever
// umask it was started with, so every file SQLite or anything else creates is
// owner-only from the first byte (read from /proc, Linux only).
func TestBinaryUmask(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := buildBinary(t)
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	cmd := exec.Command(bin)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); _ = cmd.Wait() }()
	// hello is written after the process setup.
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || !strings.Contains(line, `"hello"`) {
		t.Fatalf("first line %q %v", line, err)
	}
	status, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/status")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(l, "Umask:") {
			if v := strings.TrimSpace(strings.TrimPrefix(l, "Umask:")); v != "0077" {
				t.Fatalf("umask %s, want 0077", v)
			}
			return
		}
	}
	t.Fatal("no Umask line")
}
