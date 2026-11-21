package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrRefused is matched by RefusedError via errors.Is when a node child process
// returns a "refused …" line-protocol reply.
var ErrRefused = errors.New("the node refused the command")

// RefusedError wraps a node child process's "refused …" reply line so callers
// can distinguish an explicit node refusal from a process crash or broken pipe.
type RefusedError struct {
	Line string
}

// Error returns the verbatim "refused …" line emitted by the node process.
func (e *RefusedError) Error() string { return e.Line }

// Is allows callers to match any RefusedError with errors.Is(err, ErrRefused).
func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

// Remote invokes relocation, claim, status, and inspect operations over a live
// node child Process, implementing relocate.Participant with strict reply
// parsing and optional per-reply bounds tracking through OnReply.
type Remote struct {
	Process func() *Process
	OnReply func(command string, reply Reply)
}

func (r Remote) call(command string) (string, error) {
	p := r.Process()
	if p == nil {
		return "", fmt.Errorf("node process for %q is not running", command)
	}
	reply, err := p.Send(command)
	if err != nil {
		return "", err
	}
	if r.OnReply != nil {
		r.OnReply(command, reply)
	}
	if strings.HasPrefix(reply.Line, "refused "+command+": ") {
		return "", &RefusedError{Line: reply.Line}
	}
	if reply.Line == "ok "+command {
		return "", nil
	}
	detail, found := strings.CutPrefix(reply.Line, "ok "+command+": ")
	if !found {
		return "", fmt.Errorf("node reply %q does not answer %q", reply.Line, command)
	}
	return detail, nil
}

// scanExactly parses detail with fmt.Sscanf and re-formats the scanned values
// to reject extra tokens, padding, or truncated replies that Sscanf alone accepts.
func scanExactly(detail, format string, values ...any) error {
	if _, err := fmt.Sscanf(detail, format, values...); err != nil {
		return fmt.Errorf("node reply %q: %w", detail, err)
	}
	pointed := make([]any, len(values))
	for i, v := range values {
		switch p := v.(type) {
		case *int64:
			pointed[i] = *p
		case *uint64:
			pointed[i] = *p
		case *int:
			pointed[i] = *p
		case *string:
			pointed[i] = *p
		case *bool:
			pointed[i] = *p
		default:
			return fmt.Errorf("scanExactly: unsupported %T", v)
		}
	}
	if fmt.Sprintf(format, pointed...) != detail {
		return fmt.Errorf("node reply %q does not have the form %q", detail, format)
	}
	return nil
}

// Seal sends "seal <id>" to the child process.
func (r Remote) Seal(ctx context.Context, id string) error {
	detail, err := r.call("seal " + id)
	if err == nil && detail != "" {
		err = fmt.Errorf("node reply %q to seal", detail)
	}
	return err
}

// Drain sends "drain <id>" to the child process and parses the drained position and sha256.
func (r Remote) Drain(ctx context.Context, id string) (Drained, error) {
	var d Drained
	detail, err := r.call("drain " + id)
	if err == nil {
		err = scanExactly(detail, "position %d, sha256 %s", &d.Position, &d.SHA256)
	}
	return d, err
}

// Stage sends "stage <id> <epoch>" to the child process and parses the staged position and sha256.
func (r Remote) Stage(ctx context.Context, id string, epoch uint64) (Staged, error) {
	var s Staged
	detail, err := r.call(fmt.Sprintf("stage %s %d", id, epoch))
	if err == nil {
		err = scanExactly(detail, "position %d, sha256 %s", &s.Position, &s.SHA256)
	}
	return s, err
}

// Activate sends "activate <id> <epoch> <position> <digest>" (plus
// "without-fence" when fenceStore is false) and parses the resulting fence slot
// and adoption flag.
func (r Remote) Activate(ctx context.Context, id string, epoch uint64, position int64, digest string, fenceStore bool) (Activated, error) {
	var a Activated
	command := fmt.Sprintf("activate %s %d %d %s", id, epoch, position, digest)
	if !fenceStore {
		command += " without-fence"
	}
	detail, err := r.call(command)
	if err == nil {
		err = scanExactly(detail, "fence at position %d, adopted %t", &a.Fence, &a.Adopted)
	}
	return a, err
}

// Retire sends "retire <id>" to the child process.
func (r Remote) Retire(ctx context.Context, id string) error {
	detail, err := r.call("retire " + id)
	if err == nil && detail != "" {
		err = fmt.Errorf("node reply %q to retire", detail)
	}
	return err
}

// Discard sends "discard <id>" to the child process.
func (r Remote) Discard(ctx context.Context, id string) error {
	detail, err := r.call("discard " + id)
	if err == nil && detail != "" {
		err = fmt.Errorf("node reply %q to discard", detail)
	}
	return err
}

// Claim sends "claim <id>" to the child process and parses the ClaimResult.
func (r Remote) Claim(ctx context.Context, id string) (ClaimResult, error) {
	var c ClaimResult
	detail, err := r.call("claim " + id)
	if err == nil {
		err = scanExactly(detail, "epoch %d, restored position %d, fence at position %d, attempts %d", &c.Epoch, &c.RestoredFrom, &c.Fence, &c.Attempts)
	}
	return c, err
}

// Status sends "status <id>" to the child process and returns the status string.
func (r Remote) Status(id string) (string, error) {
	return r.call("status " + id)
}

// Inspect sends "check <id>" to the child process and parses user_version and integrity_check.
func (r Remote) Inspect(ctx context.Context, id string) (Inspection, error) {
	var i Inspection
	detail, err := r.call("check " + id)
	if err == nil {
		err = scanExactly(detail, "user_version %d, integrity %s", &i.UserVersion, &i.Integrity)
	}
	return i, err
}

// ParseAck strictly parses an "ok write <id> <mode>: …" reply line into the
// database id and Ack, rejecting malformed fields, padding, or trailing text.
func ParseAck(line string) (string, Ack, error) {
	rest, found := strings.CutPrefix(line, "ok write ")
	head, detail, cut := strings.Cut(rest, ": ")
	fields := strings.Fields(head)
	if !found || !cut || len(fields) != 2 || head != fields[0]+" "+fields[1] {
		return "", Ack{}, fmt.Errorf("not an acknowledgement: %q", line)
	}
	ack := Ack{Mode: AckMode(fields[1])}
	var err error
	switch ack.Mode {
	case LocalDurable:
		err = scanExactly(detail, "acknowledged at epoch %d, user_version %d, committed locally (not yet in the store)", &ack.Epoch, &ack.UserVersion)
	case StoreDurable:
		err = scanExactly(detail, "acknowledged at epoch %d, user_version %d, stored through position %d", &ack.Epoch, &ack.UserVersion, &ack.Position)
	default:
		err = fmt.Errorf("unknown acknowledgement mode in %q", line)
	}
	return fields[0], ack, err
}
