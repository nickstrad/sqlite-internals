package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
)

const (
	RelocationRunning    = "running"
	RelocationDone       = "done"
	RelocationRolledBack = "rolled-back"
	RelocationSuperseded = "superseded"
)

var (
	ErrNotTheOwner        = errors.New("catalog: the database is not owned by that node")
	ErrRelocationRunning  = errors.New("catalog: a relocation of the database is already running")
	ErrNoRelocation       = errors.New("catalog: no running relocation of the database")
	ErrRelocationStepGoes = errors.New("catalog: a relocation can only record a step at most one past its last")
	sha256Hex             = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const relocationSchema = `CREATE TABLE IF NOT EXISTS relocations (
	id TEXT PRIMARY KEY,
	source TEXT NOT NULL,
	destination TEXT NOT NULL,
	from_epoch INTEGER NOT NULL,
	to_epoch INTEGER NOT NULL,
	step INTEGER NOT NULL,
	position INTEGER NOT NULL DEFAULT 0,
	sha256 TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL
) STRICT`

type Relocation struct {
	ID          string
	Source      string
	Destination string
	FromEpoch   uint64
	ToEpoch     uint64
	Step        int
	Position    int64
	SHA256      string
	State       string
}

func (r Relocation) String() string {
	return fmt.Sprintf("relocation of %s from %s (epoch %d) to %s (epoch %d): step %d, position %d, state %s",
		r.ID, r.Source, r.FromEpoch, r.Destination, r.ToEpoch, r.Step, r.Position, r.State)
}

func (c *Catalog) ensureRelocations(ctx context.Context) error {
	_, err := c.DB.ExecContext(ctx, relocationSchema)
	return err
}

func (c *Catalog) BeginRelocation(ctx context.Context, id, source, destination string) (Relocation, error) {
	if source == destination || source == "" || destination == "" {
		return Relocation{}, fmt.Errorf("catalog: relocate %s from %q to %q", id, source, destination)
	}
	if err := c.ensureRelocations(ctx); err != nil {
		return Relocation{}, err
	}
	tx, err := c.DB.BeginTx(ctx, nil)
	if err != nil {
		return Relocation{}, err
	}
	defer tx.Rollback()
	var owner string
	var epoch uint64
	err = tx.QueryRowContext(ctx, "SELECT owner, epoch FROM databases WHERE id = ?", id).Scan(&owner, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return Relocation{}, fmt.Errorf("%w: %s", ErrUnknown, id)
	}
	if err != nil {
		return Relocation{}, err
	}
	if owner != source || epoch == 0 {
		return Relocation{}, fmt.Errorf("%w: %s is owned by %q at epoch %d, not by %q", ErrNotTheOwner, id, owner, epoch, source)
	}
	var running int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM relocations WHERE id = ? AND state = ?", id, RelocationRunning).Scan(&running); err != nil {
		return Relocation{}, err
	}
	if running != 0 {
		return Relocation{}, fmt.Errorf("%w: %s", ErrRelocationRunning, id)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE databases SET owner = ?, epoch = epoch + 1, location = '' WHERE id = ?", destination, id); err != nil {
		return Relocation{}, err
	}
	r := Relocation{ID: id, Source: source, Destination: destination, FromEpoch: epoch, ToEpoch: epoch + 1, Step: 1, State: RelocationRunning}
	if _, err := tx.ExecContext(ctx, "INSERT OR REPLACE INTO relocations (id, source, destination, from_epoch, to_epoch, step, position, sha256, state) VALUES (?, ?, ?, ?, ?, 1, 0, '', ?)",
		id, source, destination, r.FromEpoch, r.ToEpoch, RelocationRunning); err != nil {
		return Relocation{}, err
	}
	return r, tx.Commit()
}

func (c *Catalog) GetRelocation(ctx context.Context, id string) (Relocation, error) {
	if err := c.ensureRelocations(ctx); err != nil {
		return Relocation{}, err
	}
	r := Relocation{ID: id}
	err := c.DB.QueryRowContext(ctx, "SELECT source, destination, from_epoch, to_epoch, step, position, sha256, state FROM relocations WHERE id = ?", id).
		Scan(&r.Source, &r.Destination, &r.FromEpoch, &r.ToEpoch, &r.Step, &r.Position, &r.SHA256, &r.State)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("%w: %s", ErrNoRelocation, id)
	}
	return r, err
}

func (c *Catalog) AdvanceRelocation(ctx context.Context, id string, step int, position int64, digest string) error {
	if position < 0 || (digest != "" && !sha256Hex.MatchString(digest)) {
		return fmt.Errorf("catalog: relocation of %s: position %d, sha256 %q", id, position, digest)
	}
	if err := c.ensureRelocations(ctx); err != nil {
		return err
	}
	result, err := c.DB.ExecContext(ctx, `UPDATE relocations SET step = max(step, ?),
		position = CASE WHEN ? > 0 THEN ? ELSE position END,
		sha256 = CASE WHEN ? <> '' THEN ? ELSE sha256 END
		WHERE id = ? AND state = ? AND ? BETWEEN 1 AND step + 1`,
		step, position, position, digest, digest, id, RelocationRunning, step)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: %s to step %d", ErrRelocationStepGoes, id, step)
	}
	return nil
}

func (c *Catalog) FinishRelocation(ctx context.Context, id, state string) error {
	if state != RelocationDone && state != RelocationRolledBack && state != RelocationSuperseded {
		return fmt.Errorf("catalog: %q is not a final relocation state", state)
	}
	if err := c.ensureRelocations(ctx); err != nil {
		return err
	}
	result, err := c.DB.ExecContext(ctx, "UPDATE relocations SET state = ? WHERE id = ? AND state = ?", state, id, RelocationRunning)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: %s", ErrNoRelocation, id)
	}
	return nil
}
