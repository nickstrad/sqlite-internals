//go:build !linux

package trace

import (
	"context"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

// Supported is false outside Linux, where strace is not available.
const Supported = false

// StracePath always returns ErrLinuxOnly on non-Linux platforms.
func StracePath() (string, error) {
	return "", ErrLinuxOnly
}

// Run always returns ErrLinuxOnly on non-Linux platforms.
func Run(ctx context.Context, options Options) (*Result, error) {
	return nil, ErrLinuxOnly
}

// RunCLIMerged always returns ErrLinuxOnly on non-Linux platforms.
func RunCLIMerged(ctx context.Context, cli sqlitex.CLI, request CLITrace, options Options) (*Merged, *Result, error) {
	return nil, nil, ErrLinuxOnly
}
