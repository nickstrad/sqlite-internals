package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickstrad/walfleet/internal/procs"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/walfile"
)

func clean(s string) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "/")
	return strings.Join(strings.Fields(s), " ")
}

func report(id, tried, expect, got string, held bool) {
	status := "HELD"
	if !held {
		status = "BROKEN"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, clean(tried), clean(expect), clean(got), status)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func mustDo(script *procs.Script, c *procs.Child, action string, args ...string) procs.Result {
	r, err := script.Do(c, action, args...)
	if err != nil {
		fatal(c.Name+" "+action, err)
	}
	if !r.OK() {
		fatal(c.Name+" "+action, fmt.Errorf("unexpected %s (%s)", r.Outcome(), r.Error))
	}
	return r
}

// copyChunked reads src in 4 KiB chunks with a tiny yield between chunks so unscripted
// concurrent copies realistically span in-flight writes (simulating cp / tar / rsync).
func copyChunked(src, dst string, delay time.Duration) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	buf := make([]byte, 4096)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				return werr
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	return out.Close()
}

// attackTorn01a runs an unscripted concurrent copy race in rollback mode while a
// writer child continuously commits recipe steps 6..25.
func attackTorn01a(ctx context.Context, root string) {
	r := recipe.Default().WithJournalMode(sqlitex.JournalDelete)
	dir := filepath.Join(root, "torn01a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("torn01a mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("torn01a build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalDelete}.Args()...)
	if err != nil {
		fatal("torn01a spawn", err)
	}

	// Drive writer steps 6..25 in a goroutine while parent (which holds NO SQLite
	// connection to db) takes unscripted 4 KiB chunked copies.
	writerDone := make(chan error, 1)
	go func() {
		for k := 6; k <= 25; k++ {
			res, err := script.Do(writer, "recipe-step", strconv.Itoa(k))
			if err != nil {
				writerDone <- err
				return
			}
			if !res.OK() {
				writerDone <- fmt.Errorf("step %d: %s", k, res.Outcome())
				return
			}
			time.Sleep(3 * time.Millisecond)
		}
		writerDone <- nil
	}()

	const maxCopies = 30
	var copies []string
	for i := 0; i < maxCopies; i++ {
		select {
		case err := <-writerDone:
			if err != nil {
				fatal("torn01a writer", err)
			}
			writerDone = nil
		default:
		}
		if writerDone == nil {
			break
		}
		dst := filepath.Join(dir, fmt.Sprintf("copy-%02d.db", i))
		if err := copyChunked(db, dst, 150*time.Microsecond); err != nil {
			fatal("torn01a copy", err)
		}
		copies = append(copies, dst)
	}
	if writerDone != nil {
		if err := <-writerDone; err != nil {
			fatal("torn01a writer wait", err)
		}
	}
	if err := script.Close(); err != nil {
		fatal("torn01a close", err)
	}

	okAndMatched, corruptOrNoMatch := 0, 0
	for _, p := range copies {
		insp, err := procs.InspectDatabase(ctx, p, sqlitex.JournalDelete, "PRAGMA user_version")
		if err != nil {
			fatal("torn01a inspect", err)
		}
		match, err := r.MatchStep(ctx, p)
		if err != nil {
			fatal("torn01a match", err)
		}
		if insp.OpenError == "" && insp.Integrity == "ok" && match.Found() {
			okAndMatched++
		} else {
			corruptOrNoMatch++
		}
	}
	// Outline 6.3: claim is "not safe, not always corrupt". Under an active writer,
	// some copies may pass by luck, others tear; we expect at least 1 copy taken and
	// document the observed mix (if 0 corrupt copies occurred across the entire run,
	// note that luck hid the race; if >=1 corrupt, claim directly confirmed).
	held := len(copies) > 0 && corruptOrNoMatch > 0
	report(
		"TORN-01a",
		fmt.Sprintf("unscripted 4KiB chunked copies (%d taken) during continuous rollback commits (steps 6-25)", len(copies)),
		"not safe: at least one live copy fails integrity_check or matches no recipe state (some may pass by luck)",
		fmt.Sprintf("copies=%d ok_and_matched=%d corrupt_or_nomatch=%d", len(copies), okAndMatched, corruptOrNoMatch),
		held,
	)
}

// attackTorn01b runs an unscripted concurrent copy race of the main .db file in WAL mode
// with wal_autocheckpoint=10 so checkpoints interleave with copies.
func attackTorn01b(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "torn01b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("torn01b mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("torn01b build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{
		Path:              db,
		JournalMode:       sqlitex.JournalWAL,
		WALAutocheckpoint: 10,
	}.Args()...)
	if err != nil {
		fatal("torn01b spawn", err)
	}

	var latestCommitted int
	writerDone := make(chan error, 1)
	go func() {
		for k := 6; k <= 25; k++ {
			res, err := script.Do(writer, "recipe-step", strconv.Itoa(k))
			if err != nil {
				writerDone <- err
				return
			}
			if !res.OK() {
				writerDone <- fmt.Errorf("step %d: %s", k, res.Outcome())
				return
			}
			latestCommitted = k
			time.Sleep(3 * time.Millisecond)
		}
		writerDone <- nil
	}()

	const maxCopies = 30
	type sample struct {
		path string
		atK  int
	}
	var samples []sample
	for i := 0; i < maxCopies; i++ {
		select {
		case err := <-writerDone:
			if err != nil {
				fatal("torn01b writer", err)
			}
			writerDone = nil
		default:
		}
		if writerDone == nil {
			break
		}
		dst := filepath.Join(dir, fmt.Sprintf("copy-%02d.db", i))
		at := latestCommitted
		if err := copyChunked(db, dst, 150*time.Microsecond); err != nil {
			fatal("torn01b copy", err)
		}
		samples = append(samples, sample{path: dst, atK: at})
	}
	if writerDone != nil {
		if err := <-writerDone; err != nil {
			fatal("torn01b writer wait", err)
		}
	}
	if err := script.Close(); err != nil {
		fatal("torn01b close", err)
	}

	okMatched, okStale, corruptOrNoMatch := 0, 0, 0
	for _, s := range samples {
		insp, err := procs.InspectDatabase(ctx, s.path, sqlitex.JournalWAL, "PRAGMA user_version")
		if err != nil {
			fatal("torn01b inspect", err)
		}
		match, err := r.MatchStep(ctx, s.path)
		if err != nil {
			fatal("torn01b match", err)
		}
		if insp.OpenError == "" && insp.Integrity == "ok" && match.Found() {
			okMatched++
			if match.Step < s.atK {
				okStale++
			}
		} else {
			corruptOrNoMatch++
		}
	}
	// Every WAL main-only live copy is unsafe: either torn by an autocheckpoint or stale.
	held := len(samples) > 0 && (corruptOrNoMatch > 0 || okStale > 0)
	report(
		"TORN-01b",
		fmt.Sprintf("unscripted 4KiB copies (%d taken) of WAL main .db under active commits with wal_autocheckpoint=10", len(samples)),
		"copies are unsafe: torn mid-checkpoint or stale behind committed steps",
		fmt.Sprintf("copies=%d ok_matched=%d (of_which_stale=%d) corrupt_or_nomatch=%d", len(samples), okMatched, okStale, corruptOrNoMatch),
		held,
	)
}

// buildLedgerDB creates a database with two separate tables separated by ~30 spacer
// pages so ledger_a lives in the first half of the file and ledger_b in the second half.
func buildLedgerDB(ctx context.Context, path, journal string) (int64, int) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fatal("ledger mkdir", err)
	}
	stmts := []string{
		"CREATE TABLE ledger_a(id INTEGER PRIMARY KEY, bal INTEGER NOT NULL)",
		"INSERT INTO ledger_a VALUES (1, 500)",
		"CREATE TABLE spacer(id INTEGER PRIMARY KEY, pad TEXT NOT NULL)",
	}
	pad := strings.Repeat("S", 3500)
	for i := 1; i <= 30; i++ {
		stmts = append(stmts, fmt.Sprintf("INSERT INTO spacer VALUES (%d, '%s')", i, pad))
	}
	stmts = append(stmts,
		"CREATE TABLE ledger_b(id INTEGER PRIMARY KEY, bal INTEGER NOT NULL)",
		"INSERT INTO ledger_b VALUES (1, 500)",
	)
	if err := procs.CreateDatabase(ctx, path, journal, stmts...); err != nil {
		fatal("ledger create", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		fatal("ledger stat", err)
	}
	pages := int(info.Size() / 4096)
	halfBytes := int64(pages/2) * 4096
	return halfBytes, pages
}

// attackTorn02a demonstrates a scripted split-half copy that HAPPENS TO PASS
// integrity_check=ok and equals an exact committed state (only the already-copied
// first half changed between the two halves).
func attackTorn02a(ctx context.Context, root string) {
	dir := filepath.Join(root, "torn02a")
	db := filepath.Join(dir, "ledger.db")
	halfBytes, _ := buildLedgerDB(ctx, db, sqlitex.JournalWAL)

	script := procs.NewScript(io.Discard)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("torn02a spawn w", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
	if err != nil {
		fatal("torn02a spawn c", err)
	}

	// Copy half 1 (containing ledger_a=500).
	mustDo(script, copier, "copy-range", "db", "lucky.db", "0", strconv.FormatInt(halfBytes, 10))
	// Update ONLY ledger_a (in half 1, already copied) and checkpoint into db file!
	mustDo(script, writer, "begin", "immediate")
	mustDo(script, writer, "exec", "UPDATE ledger_a SET bal = 999 WHERE id = 1")
	mustDo(script, writer, "commit")
	mustDo(script, writer, "checkpoint", sqlitex.CheckpointPassive)
	// Copy half 2 (unchanged).
	mustDo(script, copier, "copy-range", "db", "lucky.db", strconv.FormatInt(halfBytes, 10), "end")
	if err := script.Close(); err != nil {
		fatal("torn02a close", err)
	}

	insp, err := procs.InspectDatabase(ctx, filepath.Join(dir, "lucky.db"), sqlitex.JournalWAL,
		"SELECT (SELECT bal FROM ledger_a WHERE id=1) || ',' || (SELECT bal FROM ledger_b WHERE id=1)")
	if err != nil {
		fatal("torn02a inspect", err)
	}
	// Passing happens by luck because the mutated page was in the already-copied half,
	// yielding a stale-but-valid old state (500,500) even though (999,500) was committed and checkpointed.
	held := insp.OpenError == "" && insp.Integrity == "ok" && insp.Values[0] == "500,500"
	report(
		"TORN-02a",
		"scripted split-half copy across a WAL checkpoint that only dirtied pages in half 1",
		"lucky copy passes integrity_check=ok and equals old state (500,500) despite committed (999,500), showing passing is luck not safety",
		fmt.Sprintf("integrity=%s balances=(%s)", insp.Integrity, insp.Values[0]),
		held,
	)
}

// attackTorn02b demonstrates the most dangerous live-copy failure:
// a split-half copy across an atomic 2-table transfer (ledger_a in half 1, ledger_b in half 2)
// that PASSES PRAGMA integrity_check = ok YET VIOLATES TRANSACTION ATOMICITY (a+b=1100 != 1000)!
func attackTorn02b(ctx context.Context, root string) {
	runMode := func(journal string) (string, string, bool) {
		dir := filepath.Join(root, "torn02b-"+journal)
		db := filepath.Join(dir, "ledger.db")
		halfBytes, _ := buildLedgerDB(ctx, db, journal)

		script := procs.NewScript(io.Discard)
		defer script.Close()
		writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: journal}.Args()...)
		if err != nil {
			fatal("torn02b spawn w", err)
		}
		copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
		if err != nil {
			fatal("torn02b spawn c", err)
		}

		// Copy half 1 (ledger_a = 500).
		mustDo(script, copier, "copy-range", "db", "split.db", "0", strconv.FormatInt(halfBytes, 10))

		// Atomic transfer of 100 from ledger_a to ledger_b (invariant: a + b == 1000).
		mustDo(script, writer, "begin", "immediate")
		mustDo(script, writer, "exec", "UPDATE ledger_a SET bal = 400 WHERE id = 1")
		mustDo(script, writer, "exec", "UPDATE ledger_b SET bal = 600 WHERE id = 1")
		mustDo(script, writer, "commit")
		if journal == sqlitex.JournalWAL {
			mustDo(script, writer, "checkpoint", sqlitex.CheckpointPassive)
		}

		// Copy half 2 (ledger_b = 600).
		mustDo(script, copier, "copy-range", "db", "split.db", strconv.FormatInt(halfBytes, 10), "end")
		if err := script.Close(); err != nil {
			fatal("torn02b close", err)
		}

		insp, err := procs.InspectDatabase(ctx, filepath.Join(dir, "split.db"), journal,
			"SELECT (SELECT bal FROM ledger_a WHERE id=1) || '+' || (SELECT bal FROM ledger_b WHERE id=1) || '=' || ((SELECT bal FROM ledger_a WHERE id=1)+(SELECT bal FROM ledger_b WHERE id=1))")
		if err != nil {
			fatal("torn02b inspect", err)
		}
		// Both B-trees are structurally intact (integrity_check=ok!), yet sum=1100 != 1000!
		silentTear := insp.OpenError == "" && insp.Integrity == "ok" && insp.Values[0] == "500+600=1100"
		return insp.Integrity, insp.Values[0], silentTear
	}

	intWAL, sumWAL, tearWAL := runMode(sqlitex.JournalWAL)
	intDel, sumDel, tearDel := runMode(sqlitex.JournalDelete)

	// Chapter 6.3 claims: "copying a database file while it is being written does not give a database"
	// (evidence checks integrity_check != ok OR !match.Found()).
	// Here we confirm that a torn copy CAN pass PRAGMA integrity_check=ok while holding
	// an impossible non-atomic state (500+600=1100), proving integrity_check alone is blind to torn copies!
	held := tearWAL && tearDel
	report(
		"TORN-02b",
		"split-half copy across an atomic 2-table transfer (a:500->400 in half 1, b:500->600 in half 2) in WAL and rollback",
		"torn copy PASSES integrity_check=ok yet violates atomicity (500+600=1100 != 1000), proving integrity_check alone misses torn copies",
		fmt.Sprintf("wal(integrity=%s sum=%s) delete(integrity=%s sum=%s)", intWAL, sumWAL, intDel, sumDel),
		held,
	)
}

// attackTorn03 verifies that even under SQLite's default wal_autocheckpoint=1000,
// copying the main .db file alone silently loses recent commits while passing integrity_check.
func attackTorn03(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "torn03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("torn03 mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("torn03 build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{
		Path:              db,
		JournalMode:       sqlitex.JournalWAL,
		WALAutocheckpoint: 1000, // SQLite default
	}.Args()...)
	if err != nil {
		fatal("torn03 spawn w", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
	if err != nil {
		fatal("torn03 spawn c", err)
	}
	for k := 6; k <= 10; k++ {
		mustDo(script, writer, "recipe-step", strconv.Itoa(k))
	}
	mustDo(script, copier, "copy", "db", "main-only.db")
	if err := script.Close(); err != nil {
		fatal("torn03 close", err)
	}

	copyPath := filepath.Join(dir, "main-only.db")
	insp, err := procs.InspectDatabase(ctx, copyPath, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("torn03 inspect", err)
	}
	match, err := r.MatchStep(ctx, copyPath)
	if err != nil {
		fatal("torn03 match", err)
	}
	held := insp.Integrity == "ok" && insp.Values[0] == "5" && match.Found() && match.Step == 5
	report(
		"TORN-03",
		"WAL main-file-only copy with default wal_autocheckpoint=1000 after committing steps 6-10",
		"opens cleanly (integrity_check=ok) but equals stale step 5, silently losing all 5 commits (6-10)",
		fmt.Sprintf("integrity=%s user_version=%s match=%s", insp.Integrity, insp.Values[0], match.String()),
		held,
	)
}

// attackTorn04a tests copying .db then -wal WITHOUT a held read transaction while
// checkpoint(TRUNCATE) runs in between: the replay silently loses the latest commit.
func attackTorn04a(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "torn04a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("torn04a mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 8); err != nil {
		fatal("torn04a build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("torn04a spawn w", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
	if err != nil {
		fatal("torn04a spawn c", err)
	}

	// Writer commits step 9 (still in -wal; main .db is step 8).
	mustDo(script, writer, "recipe-step", "9")
	// Copier copies main .db (step 8) WITHOUT holding a read transaction!
	mustDo(script, copier, "copy", "db", "copy.db")
	// Writer checkpoints TRUNCATE: writes step 9 to .db and truncates -wal to 0 bytes.
	ckpt := mustDo(script, writer, "checkpoint", sqlitex.CheckpointTruncate)
	// Copier now copies -wal (0 bytes).
	walCopy := mustDo(script, copier, "copy", "-wal", "copy.wal")
	if err := script.Close(); err != nil {
		fatal("torn04a close", err)
	}

	replayed := filepath.Join(dir, "replayed.db")
	if _, err := walfile.ReplayFiles(filepath.Join(dir, "copy.db"), filepath.Join(dir, "copy.wal"), replayed); err != nil {
		fatal("torn04a replay", err)
	}
	insp, err := procs.InspectDatabase(ctx, replayed, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("torn04a inspect", err)
	}
	match, err := r.MatchStep(ctx, replayed)
	if err != nil {
		fatal("torn04a match", err)
	}
	held := insp.Integrity == "ok" && match.Found() && match.Step == 8
	report(
		"TORN-04a",
		"naive .db + -wal copy WITHOUT held read txn while checkpoint(TRUNCATE) runs between copies",
		"replay yields stale step 8 (silently losing committed step 9 because -wal was truncated to 0 bytes)",
		fmt.Sprintf("ckpt=(%s) wal_copy=(%s) integrity=%s match=%s", ckpt.Value, walCopy.Value, insp.Integrity, match.String()),
		held,
	)
}

// attackTorn04b tests copying .db then -wal WITHOUT a held read transaction while
// checkpoint(RESTART) + a new commit overwrites the WAL with new salts!
// This produces a genuinely CORRUPT / mismatched replay, proving why 6.4's held read txn is essential.
func attackTorn04b(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "torn04b")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("torn04b mkdir", err)
	}
	db := filepath.Join(dir, "r.db")
	if err := r.Build(ctx, db, 5); err != nil {
		fatal("torn04b build", err)
	}

	script := procs.NewScript(io.Discard)
	defer script.Close()
	writer, err := script.Spawn("writer", procs.RoleSQL, procs.SQLOptions{Path: db, JournalMode: sqlitex.JournalWAL}.Args()...)
	if err != nil {
		fatal("torn04b spawn w", err)
	}
	copier, err := script.Spawn("copier", procs.RoleCopier, "-db", db, "-out", dir)
	if err != nil {
		fatal("torn04b spawn c", err)
	}

	// Steps 6..8 live ONLY in -wal; main .db is still at step 5.
	for k := 6; k <= 8; k++ {
		mustDo(script, writer, "recipe-step", strconv.Itoa(k))
	}
	// Copier copies main .db (step 5) with NO held read transaction.
	mustDo(script, copier, "copy", "db", "copy.db")
	// Writer checkpoints RESTART (backfills 6..8 into .db) and commits step 9,
	// which restarts -wal at frame 1 with NEW salts, erasing steps 6..8 from -wal!
	ckpt := mustDo(script, writer, "checkpoint", sqlitex.CheckpointRestart)
	mustDo(script, writer, "recipe-step", "9")
	mustDo(script, copier, "copy", "-wal", "copy.wal")
	if err := script.Close(); err != nil {
		fatal("torn04b close", err)
	}

	replayed := filepath.Join(dir, "replayed.db")
	rep, rerr := walfile.ReplayFiles(filepath.Join(dir, "copy.db"), filepath.Join(dir, "copy.wal"), replayed)
	if rerr != nil {
		// Replay itself rejecting the incompatible base+WAL pair also proves detection.
		report(
			"TORN-04b",
			"naive .db + -wal copy WITHOUT held read txn while checkpoint(RESTART) + step 9 restarts WAL between copies",
			"replay fails or produces corrupt/non-recipe DB because steps 6-8 vanished between .db and restarted -wal",
			fmt.Sprintf("ckpt=(%s) replay_err=%v", ckpt.Value, rerr),
			true,
		)
		return
	}
	insp, err := procs.InspectDatabase(ctx, replayed, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("torn04b inspect", err)
	}
	match, err := r.MatchStep(ctx, replayed)
	if err != nil {
		fatal("torn04b match", err)
	}
	corrupt := insp.OpenError != "" || insp.Integrity != "ok" || !match.Found()
	report(
		"TORN-04b",
		"naive .db + -wal copy WITHOUT held read txn while checkpoint(RESTART) + step 9 restarts WAL between copies",
		"replayed DB is corrupt or matches no recipe state (steps 6-8 lost between base copy and restarted -wal)",
		fmt.Sprintf("ckpt=(%s) replayed_frames=%d open_err=%q integrity=%s user_version=%s match=%s", ckpt.Value, rep.AppliedFrames, insp.OpenError, procs.IntegritySummary(insp.Integrity), insp.Values[0], match.String()),
		corrupt,
	)
}

// attackTorn05 is the must-be-able-to-fail check on 6.3's verifier: inject 1-byte
// B-tree corruption into a valid closed step-8 DB and confirm BOTH InspectDatabase
// and recipe.MatchStep catch it.
func attackTorn05(ctx context.Context, root string) {
	r := recipe.Default()
	dir := filepath.Join(root, "torn05")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("torn05 mkdir", err)
	}
	db := filepath.Join(dir, "step8.db")
	if err := r.Build(ctx, db, 8); err != nil {
		fatal("torn05 build", err)
	}
	data, err := os.ReadFile(db)
	if err != nil {
		fatal("torn05 read", err)
	}
	// Corrupt the B-tree page type flag byte of page 2 (offset 4096) so integrity_check must fail.
	if len(data) < 8192 {
		fatal("torn05 size", fmt.Errorf("unexpected db size %d", len(data)))
	}
	data[4096] = 0xFF
	tampered := filepath.Join(dir, "tampered.db")
	if err := os.WriteFile(tampered, data, 0o644); err != nil {
		fatal("torn05 write", err)
	}

	insp, err := procs.InspectDatabase(ctx, tampered, sqlitex.JournalWAL, "PRAGMA user_version")
	if err != nil {
		fatal("torn05 inspect", err)
	}
	match, err := r.MatchStep(ctx, tampered)
	if err != nil {
		fatal("torn05 match", err)
	}
	held := (insp.OpenError != "" || insp.Integrity != "ok") && !match.Found()
	report(
		"TORN-05",
		"must-be-able-to-fail: flip page 2 type byte to 0xFF in a valid step-8 copy",
		"InspectDatabase reports integrity problem and recipe.MatchStep returns no match",
		fmt.Sprintf("open_err=%q integrity=%s match=%s", insp.OpenError, procs.IntegritySummary(insp.Integrity), match.String()),
		held,
	)
}

func main() {
	if procs.Serve(procs.Roles) {
		return
	}
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	workRoot := filepath.Join(repoRoot, "_work", "adv-ch06", "torn-copy-attacks")
	if err := os.RemoveAll(workRoot); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		fatal("mkdir work dir", err)
	}

	attackTorn01a(ctx, workRoot)
	attackTorn01b(ctx, workRoot)
	attackTorn02a(ctx, workRoot)
	attackTorn02b(ctx, workRoot)
	attackTorn03(ctx, workRoot)
	attackTorn04a(ctx, workRoot)
	attackTorn04b(ctx, workRoot)
	attackTorn05(ctx, workRoot)
}
