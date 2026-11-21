package node

import (
	"context"
	"fmt"
	"strconv"
)

// handleRelocation dispatches the relocation participant commands (seal, drain,
// stage, activate [without-fence], retire, discard) plus status and check for
// Serve's line protocol. The optional "without-fence" argument on activate
// supports Experiment 10.3's skip-fence self-test mutant.
func (n *Node) handleRelocation(ctx context.Context, command string, fields []string) string {
	refused := func(err error) string { return fmt.Sprintf("refused %s: %s", command, Explain(err)) }
	switch {
	case fields[0] == "seal" && len(fields) == 2:
		if err := n.Seal(ctx, fields[1]); err != nil {
			return refused(err)
		}
		return "ok " + command
	case fields[0] == "drain" && len(fields) == 2:
		drained, err := n.Drain(ctx, fields[1])
		if err != nil {
			return refused(err)
		}
		return fmt.Sprintf("ok %s: position %d, sha256 %s", command, drained.Position, drained.SHA256)
	case fields[0] == "stage" && len(fields) == 3:
		epoch, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return refused(err)
		}
		staged, err := n.Stage(ctx, fields[1], epoch)
		if err != nil {
			return refused(err)
		}
		return fmt.Sprintf("ok %s: position %d, sha256 %s", command, staged.Position, staged.SHA256)
	case fields[0] == "activate" && (len(fields) == 5 || len(fields) == 6 && fields[5] == "without-fence"):
		epoch, epochErr := strconv.ParseUint(fields[2], 10, 64)
		position, positionErr := strconv.ParseInt(fields[3], 10, 64)
		if epochErr != nil || positionErr != nil {
			return refused(fmt.Errorf("activate needs an epoch and a position"))
		}
		activated, err := n.Activate(ctx, fields[1], epoch, position, fields[4], len(fields) == 5)
		if err != nil {
			return refused(err)
		}
		return fmt.Sprintf("ok %s: fence at position %d, adopted %t", command, activated.Fence, activated.Adopted)
	case fields[0] == "retire" && len(fields) == 2:
		if err := n.Retire(ctx, fields[1]); err != nil {
			return refused(err)
		}
		return "ok " + command
	case fields[0] == "discard" && len(fields) == 2:
		if err := n.Discard(ctx, fields[1]); err != nil {
			return refused(err)
		}
		return "ok " + command
	case fields[0] == "status" && len(fields) == 2:
		return fmt.Sprintf("ok %s: %s", command, n.Status(fields[1]))
	case fields[0] == "check" && len(fields) == 2:
		inspection, err := n.Inspect(ctx, fields[1])
		if err != nil {
			return refused(err)
		}
		return fmt.Sprintf("ok %s: user_version %d, integrity %s", command, inspection.UserVersion, inspection.Integrity)
	}
	return fmt.Sprintf("refused %s: unknown command", command)
}
