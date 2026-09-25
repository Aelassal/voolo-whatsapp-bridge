// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 The voolo-whatsapp-bridge authors

package logx

import (
	waLog "go.mau.fi/whatsmeow/util/log"
)

// WA adapts Logger to whatsmeow's logger interface. Only warnings and errors are
// kept, and only their module name and constant format string: the arguments,
// which can carry JIDs, message ids or text, are dropped unread.
type WA struct {
	l      *Logger
	module string
}

var _ waLog.Logger = (*WA)(nil)

// NewWA returns a whatsmeow logger backed by l.
func NewWA(l *Logger, module string) *WA { return &WA{l: l, module: module} }

func (w *WA) emit(level Level, format string) {
	w.l.Log(level, "whatsmeow", Code(w.module+": "+format))
}

// Warnf keeps the format string only.
func (w *WA) Warnf(format string, _ ...any) { w.emit(Warn, format) }

// Errorf keeps the format string only.
func (w *WA) Errorf(format string, _ ...any) { w.emit(Error, format) }

// Infof is dropped.
func (w *WA) Infof(string, ...any) {}

// Debugf is dropped.
func (w *WA) Debugf(string, ...any) {}

// Sub returns a logger for a sub-module.
func (w *WA) Sub(module string) waLog.Logger {
	m := module
	if w.module != "" {
		m = w.module + "/" + module
	}
	return &WA{l: w.l, module: m}
}
