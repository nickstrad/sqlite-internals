package node

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrRefused = errors.New("the node refused the command")

type RefusedError struct {
	Line string
}

func (e *RefusedError) Error() string { return e.Line }

func (e *RefusedError) Is(target error) bool { return target == ErrRefused }

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

func (r Remote) Seal(ctx context.Context, id string) error {
	detail, err := r.call("seal " + id)
	if err == nil && detail != "" {
		err = fmt.Errorf("node reply %q to seal", detail)
	}
	return err
}

func (r Remote) Drain(ctx context.Context, id string) (Drained, error) {
	var d Drained
	detail, err := r.call("drain " + id)
	if err == nil {
		err = scanExactly(detail, "position %d, sha256 %s", &d.Position, &d.SHA256)
	}
	return d, err
}

func (r Remote) Stage(ctx context.Context, id string, epoch uint64) (Staged, error) {
	var s Staged
	detail, err := r.call(fmt.Sprintf("stage %s %d", id, epoch))
	if err == nil {
		err = scanExactly(detail, "position %d, sha256 %s", &s.Position, &s.SHA256)
	}
	return s, err
}

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

func (r Remote) Retire(ctx context.Context, id string) error {
	detail, err := r.call("retire " + id)
	if err == nil && detail != "" {
		err = fmt.Errorf("node reply %q to retire", detail)
	}
	return err
}

func (r Remote) Discard(ctx context.Context, id string) error {
	detail, err := r.call("discard " + id)
	if err == nil && detail != "" {
		err = fmt.Errorf("node reply %q to discard", detail)
	}
	return err
}

func (r Remote) Claim(ctx context.Context, id string) (ClaimResult, error) {
	var c ClaimResult
	detail, err := r.call("claim " + id)
	if err == nil {
		err = scanExactly(detail, "epoch %d, restored position %d, fence at position %d, attempts %d", &c.Epoch, &c.RestoredFrom, &c.Fence, &c.Attempts)
	}
	return c, err
}

func (r Remote) Status(id string) (string, error) {
	return r.call("status " + id)
}

func (r Remote) Inspect(ctx context.Context, id string) (Inspection, error) {
	var i Inspection
	detail, err := r.call("check " + id)
	if err == nil {
		err = scanExactly(detail, "user_version %d, integrity %s", &i.UserVersion, &i.Integrity)
	}
	return i, err
}

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
