// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	bridge "github.com/Aelassal/voolo-whatsapp-bridge"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/logx"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/protocol"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/store"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/transport"
	"github.com/Aelassal/voolo-whatsapp-bridge/internal/wa"
)

// exitUsage is returned for unknown command-line arguments.
const exitUsage = 64

type deps struct {
	initTimeout time.Duration
	// tweak lets tests replace parts of the bridge configuration (a fake
	// whatsmeow client, shorter timers). Production leaves it nil.
	tweak func(cfg *wa.Config)
}

func defaultDeps() deps { return deps{initTimeout: 10 * time.Second} }

// bareVersion is the version without a leading "v" (MAJOR.MINOR.PATCH, as in
// hello.version and flags.json maxBridgeVersion; PROTOCOL.md §5.1).
func bareVersion() string { return strings.TrimPrefix(version, "v") }

func sourceURL() string {
	if version == "" || strings.Contains(version, "dev") {
		return bridge.SourceRepo
	}
	return bridge.SourceRepo + "/tree/v" + bareVersion()
}

func flags(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: voolo-whatsapp-bridge [--version | --license | --source]")
		return exitUsage
	}
	switch args[0] {
	case "--version", "-version":
		fmt.Fprintln(stdout, "voolo-whatsapp-bridge "+bareVersion()+" (protocol 1)")
	case "--license", "-license":
		fmt.Fprintln(stdout, "voolo-whatsapp-bridge is free software under the GNU General Public License v3.0 or later.")
		fmt.Fprintln(stdout, "Source: "+sourceURL())
		fmt.Fprintln(stdout)
		fmt.Fprint(stdout, bridge.License)
	case "--source", "-source":
		fmt.Fprintln(stdout, sourceURL())
	default:
		fmt.Fprintln(stderr, "usage: voolo-whatsapp-bridge [--version | --license | --source]")
		return exitUsage
	}
	return 0
}

// run is the whole program: it returns the process exit code (PROTOCOL.md §9).
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, d deps) (code int) {
	if len(args) > 0 {
		return flags(args, stdout, stderr)
	}
	log := logx.New(stderr, nil)
	defer func() {
		if r := recover(); r != nil {
			log.Error("panic")
			code = wa.ExitCrash
		}
	}()
	out := protocol.NewWriter(stdout, nil)

	var b *wa.Bridge
	netOpts := transport.Options{OnBlock: func() {
		if b != nil {
			b.HostBlocked()
		}
	}}
	httpClient := transport.NewClient(netOpts)
	mediaClient := transport.NewMediaClient(netOpts)
	cfg := wa.Config{
		Out: out,
		Log: log,
		OpenStore: func(ctx context.Context, dir string, key []byte) (*store.Store, error) {
			return store.Open(ctx, dir, key, logx.NewWA(log, "Database"))
		},
		NewClient: func(st *store.Store) (wa.Client, error) {
			dev, err := st.Container.GetFirstDevice(context.Background())
			if err != nil {
				return nil, err
			}
			return wa.NewRealClient(dev, httpClient, mediaClient, logx.NewWA(log, "Client")), nil
		},
		RefreshVersion: func(ctx context.Context) error { return wa.RefreshWAVersion(ctx, httpClient) },
	}
	if d.tweak != nil {
		d.tweak(&cfg)
	}
	b = wa.New(cfg)

	if err := out.Emit(protocol.EvHello, protocol.Hello{Bridge: "voolo-whatsapp-bridge", Version: bareVersion(), Protocol: protocol.Version, OS: runtime.GOOS, Arch: runtime.GOARCH}); err != nil {
		return wa.ExitCrash
	}
	log.Info("started")

	lr := protocol.NewLineReader(stdin)
	lines := make(chan []byte)
	go func() {
		defer close(lines)
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic_recovered", logx.Code("command"))
			}
		}()
		for {
			l, err := lr.Next()
			if err != nil {
				return
			}
			lines <- l
		}
	}()

	initTimer := time.NewTimer(d.initTimeout)
	defer initTimer.Stop()
	var dropped, overlong int64
	for {
		select {
		case line, ok := <-lines:
			if n := lr.Dropped.Load(); n != overlong {
				overlong = n
				log.Warn("line_dropped", logx.Code("too_long"), logx.N(n))
			}
			if !ok {
				log.Info("stdin_eof")
				b.Stop()
				return wa.ExitOK
			}
			env, err := protocol.ParseEnvelope(line)
			var ve *protocol.VersionError
			switch {
			case errors.As(err, &ve):
				b.Reject(ve.ID, protocol.ErrUnsupportedVersion)
				continue
			case err != nil:
				dropped++
				log.Warn("line_dropped", logx.Code("invalid"), logx.N(dropped))
				continue
			}
			if b.Handle(env) {
				log.Info("shutdown")
				b.Stop()
				_ = out.Emit(protocol.EvOK, protocol.Reply{ReplyTo: env.ID})
				return wa.ExitOK
			}
		case <-initTimer.C:
			if !b.Initialized() {
				log.Error("init_timeout")
				b.Stop()
				return wa.ExitNoInit
			}
		case c := <-b.Fatal():
			b.Stop()
			return c
		}
	}
}
