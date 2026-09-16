// Package logging adapts log/slog to ports.Logger.
package logging

import (
	"io"
	"log/slog"
)

// Slog wraps a *slog.Logger.
type Slog struct{ L *slog.Logger }

// New creates a text logger at the given level.
func New(w io.Writer, level slog.Level, json bool) Slog {
	opts := &slog.HandlerOptions{Level: level}
	if json {
		return Slog{slog.New(slog.NewJSONHandler(w, opts))}
	}
	return Slog{slog.New(slog.NewTextHandler(w, opts))}
}

func (s Slog) Info(msg string, kv ...any)  { s.L.Info(msg, kv...) }
func (s Slog) Warn(msg string, kv ...any)  { s.L.Warn(msg, kv...) }
func (s Slog) Error(msg string, kv ...any) { s.L.Error(msg, kv...) }
func (s Slog) Debug(msg string, kv ...any) { s.L.Debug(msg, kv...) }

// Nop discards everything (tests).
type Nop struct{}

func (Nop) Info(string, ...any)  {}
func (Nop) Warn(string, ...any)  {}
func (Nop) Error(string, ...any) {}
func (Nop) Debug(string, ...any) {}
