package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/bounds"
	"github.com/nickstrad/walfleet/ch10-multi-database-platform-capstone/service/fence"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

// Explain formats err into stable, reader-facing text for line-protocol
// replies: FencedError's message verbatim, "not the owner" for ErrNotOwner, or
// "error: <err>" otherwise.
func Explain(err error) string {
	var fenced *fence.FencedError
	switch {
	case errors.As(err, &fenced):
		return fenced.Error()
	case errors.Is(err, ErrNotOwner):
		return "not the owner"
	}
	return "error: " + err.Error()
}

// Serve runs the node's stdin/stdout line protocol, dispatching core commands
// (claim, write, ship, close) and relocation/inspection commands (via
// handleRelocation in commands.go). It writes "ready", then for each command
// line emits any recorded "problem …" lines, one "ok …" or "refused …" reply,
// and one "bounds …" line checked against assertion. A bare "close" command
// also writes assertion.Summary() and returns nil.
func Serve(ctx context.Context, n *Node, assertion *bounds.Assertion, apply func(context.Context, *sqlitex.DB) error, in io.Reader, out io.Writer) error {
	lines := bufio.NewScanner(in)
	fmt.Fprintln(out, "ready")
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) == 0 {
			continue
		}
		reply := n.handle(ctx, fields, apply)
		for _, problem := range n.TakeProblems() {
			fmt.Fprintln(out, "problem "+problem)
		}
		fmt.Fprintln(out, reply)
		sample, err := assertion.Check()
		status := "ok"
		if err != nil {
			status = "EXCEEDED: " + err.Error()
		}
		fmt.Fprintf(out, "bounds descriptors=%d memory=%d %s\n", sample.Descriptors, sample.MemoryGrowth, status)
		// Exit only on a bare "close"; "close" with extra arguments is handled
		// as an unknown command without closing n.
		if fields[0] == "close" && len(fields) == 1 {
			fmt.Fprintln(out, assertion.Summary())
			return nil
		}
	}
	return lines.Err()
}

func (n *Node) handle(ctx context.Context, fields []string, apply func(context.Context, *sqlitex.DB) error) string {
	command := strings.Join(fields, " ")
	switch {
	case fields[0] == "claim" && len(fields) == 2:
		result, err := n.Claim(ctx, fields[1])
		if err != nil {
			return fmt.Sprintf("refused %s: %s", command, Explain(err))
		}
		return fmt.Sprintf("ok %s: epoch %d, restored position %d, fence at position %d, attempts %d", command, result.Epoch, result.RestoredFrom, result.Fence, result.Attempts)
	case fields[0] == "write" && len(fields) == 3:
		ack, err := n.Write(ctx, fields[1], AckMode(fields[2]), apply)
		if err != nil {
			return fmt.Sprintf("refused %s: %s", command, Explain(err))
		}
		if ack.Mode == LocalDurable {
			return fmt.Sprintf("ok %s: acknowledged at epoch %d, user_version %d, committed locally (not yet in the store)", command, ack.Epoch, ack.UserVersion)
		}
		return fmt.Sprintf("ok %s: acknowledged at epoch %d, user_version %d, stored through position %d", command, ack.Epoch, ack.UserVersion, ack.Position)
	case fields[0] == "ship" && len(fields) == 2:
		position, err := n.Ship(ctx, fields[1])
		if err != nil {
			return fmt.Sprintf("refused %s: %s", command, Explain(err))
		}
		return fmt.Sprintf("ok %s: stored through position %d", command, position)
	case fields[0] == "close" && len(fields) == 1:
		if err := n.Close(); err != nil {
			return fmt.Sprintf("refused %s: %s", command, Explain(err))
		}
		return "ok close"
	}
	return n.handleRelocation(ctx, command, fields)
}
