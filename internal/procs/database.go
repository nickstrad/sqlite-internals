package procs

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
)

func labOptions(journalMode string) (sqlitex.Options, error) {
	r := recipe.Default().WithJournalMode(journalMode)
	if err := r.Validate(); err != nil {
		return sqlitex.Options{}, err
	}
	options := r.SQLiteOptions()
	options.ImmediateTx = false
	return options, nil
}

// CreateDatabase removes any existing database and sidecars at path, creates a
// fresh database configured with journalMode and recipe settings, executes
// statements, closes the connection, and verifies the file is at rest.
func CreateDatabase(ctx context.Context, path, journalMode string, statements ...string) error {
	if err := recipe.RemoveDatabase(path); err != nil {
		return err
	}
	options, err := labOptions(journalMode)
	if err != nil {
		return err
	}
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			db.Close()
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	if err := db.Close(); err != nil {
		return err
	}
	return recipe.CheckAtRest(path)
}

// Inspection holds the result of opening a closed database file, running
// PRAGMA integrity_check, and evaluating caller-supplied queries.
type Inspection struct {
	OpenError string
	Integrity string
	Values    []string
}

func errorText(err error) string {
	if code, ok := sqlitex.ErrorCode(err); ok {
		return fmt.Sprintf("error %s (%d)", sqlitex.ResultCodeName(code), code)
	}
	return "error " + err.Error()
}

// InspectDatabase verifies path has no live sidecar files, opens it with
// journalMode, runs PRAGMA integrity_check and each query, and closes it.
// SQLite errors (such as SQLITE_CORRUPT or SQLITE_NOTADB on a torn copy) are
// captured as text in Inspection rather than returned as errors.
func InspectDatabase(ctx context.Context, path, journalMode string, queries ...string) (Inspection, error) {
	if err := recipe.CheckAtRest(path); err != nil {
		return Inspection{}, err
	}
	options, err := labOptions(journalMode)
	if err != nil {
		return Inspection{}, err
	}
	db, err := sqlitex.Open(ctx, path, options)
	if err != nil {
		if _, isSQLite := sqlitex.ErrorCode(err); !isSQLite {
			return Inspection{}, err
		}
		inspection := Inspection{OpenError: errorText(err), Integrity: "not run"}
		for range queries {
			inspection.Values = append(inspection.Values, "not run")
		}
		return inspection, recipe.CheckAtRest(path)
	}
	inspection := Inspection{Integrity: integrityText(ctx, db)}
	for _, query := range queries {
		var value string
		if err := db.QueryRowContext(ctx, query).Scan(&value); err != nil {
			value = errorText(err)
		}
		inspection.Values = append(inspection.Values, value)
	}
	if err := db.Close(); err != nil {
		return Inspection{}, err
	}
	return inspection, recipe.CheckAtRest(path)
}

func integrityText(ctx context.Context, db *sqlitex.DB) string {
	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return errorText(err)
	}
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return errorText(err)
		}
		lines = append(lines, line)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return errorText(err)
	}
	return strings.Join(lines, "; ")
}

// IntegrityProblems splits the joined output of PRAGMA integrity_check into
// individual diagnostic lines, skipping SQLite's "*** in database ..." header.
// It returns nil when text is "ok".
func IntegrityProblems(text string) []string {
	if text == "ok" {
		return nil
	}
	var problems []string
	for _, part := range strings.Split(text, "; ") {
		for _, line := range strings.Split(part, "\n") {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "*** in database") {
				problems = append(problems, line)
			}
		}
	}
	return problems
}

// IntegritySummary condenses PRAGMA integrity_check output to either "ok" or
// "<N> problem(s), the first: <line>".
func IntegritySummary(text string) string {
	problems := IntegrityProblems(text)
	if text == "ok" {
		return "ok"
	}
	if len(problems) == 0 {
		return text
	}
	return fmt.Sprintf("%d problem(s), the first: %s", len(problems), problems[0])
}
