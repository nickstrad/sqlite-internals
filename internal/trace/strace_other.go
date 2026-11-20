//go:build !linux

package trace

import (
	"context"

	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const Supported = false

func StracePath() (string, error) {
	return "", ErrLinuxOnly
}

func Run(ctx context.Context, options Options) (*Result, error) {
	return nil, ErrLinuxOnly
}

func RunCLIMerged(ctx context.Context, cli sqlitex.CLI, request CLITrace, options Options) (*Merged, *Result, error) {
	return nil, nil, ErrLinuxOnly
}
