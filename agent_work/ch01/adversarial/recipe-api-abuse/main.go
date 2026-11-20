package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
)

const pageSize = 4096

func report(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n", id, tried, expect, got, verdict)
}

func fatal(what string, err error) {
	fmt.Fprintf(os.Stderr, "FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func copyBytes(src, dst string) []byte {
	data, err := os.ReadFile(src)
	if err != nil {
		fatal("read "+src, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		fatal("write "+dst, err)
	}
	return data
}

func main() {
	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch01", "recipe-api-abuse")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}
	// Isolate COURSE_WORK_DIR so cache manipulation never touches _work/recipe-cache.
	workDir := filepath.Join(scratch, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("mkdir work", err)
	}
	if err := os.Setenv(sqlitex.WorkDirEnv, workDir); err != nil {
		fatal("setenv", err)
	}

	r := recipe.Default().WithSteps(10)
	baseDB := filepath.Join(scratch, "step10.db")
	if err := r.Build(ctx, baseDB, 10); err != nil {
		fatal("build step10", err)
	}

	// API-01: Must-be-able-to-fail: 1-bit flip outside change counter (header byte 68
	// user_version, and data page 10 byte 500) must make MatchStep return Step=-1.
	pFlipHdr := filepath.Join(scratch, "flip-hdr.db")
	dHdr := copyBytes(baseDB, pFlipHdr)
	dHdr[68] ^= 0x01
	if err := os.WriteFile(pFlipHdr, dHdr, 0o644); err != nil {
		fatal("write flip-hdr", err)
	}
	mHdr, err := r.MatchStep(ctx, pFlipHdr)
	if err != nil {
		fatal("MatchStep flip-hdr", err)
	}

	pFlipPage := filepath.Join(scratch, "flip-page10.db")
	dPage := copyBytes(baseDB, pFlipPage)
	dPage[9*pageSize+500] ^= 0x80
	if err := os.WriteFile(pFlipPage, dPage, 0o644); err != nil {
		fatal("write flip-page", err)
	}
	mPage, err := r.MatchStep(ctx, pFlipPage)
	if err != nil {
		fatal("MatchStep flip-page", err)
	}
	held01 := !mHdr.Found() && !mPage.Found()
	report(
		"API-01",
		"flip 1 bit in header byte 68 (user_version) and 1 bit in page 10, then call recipe.MatchStep",
		"MatchStep rejects both tampered files (Found=false, Step=-1)",
		fmt.Sprintf("hdrMatch=%+v pageMatch=%+v", mHdr, mPage),
		held01,
	)

	// API-02: Overwrite bytes 24..27 (change counter) and 92..95 (version-valid-for)
	// with inconsistent values (0xDEADBEEF vs 0x00000001). Because MaskChangeCounter
	// zeros both 4-byte spans unconditionally, does MatchStep report a masked match
	// even when the header's two counter fields disagree?
	pDesync := filepath.Join(scratch, "desync-counters.db")
	dDesync := copyBytes(baseDB, pDesync)
	binary.BigEndian.PutUint32(dDesync[24:28], 0xDEADBEEF)
	binary.BigEndian.PutUint32(dDesync[92:96], 0x00000001)
	if err := os.WriteFile(pDesync, dDesync, 0o644); err != nil {
		fatal("write desync", err)
	}
	mDesync, err := r.MatchStep(ctx, pDesync)
	if err != nil {
		fatal("MatchStep desync", err)
	}
	report(
		"API-02",
		"corrupt header bytes 24..27=0xDEADBEEF and 92..95=0x00000001 (disagreeing counters), call recipe.MatchStep",
		"MatchStep rejects a file whose change_counter and version_valid_for disagree",
		fmt.Sprintf("Step=%d Exact=%v (%s)", mDesync.Step, mDesync.Exact, mDesync),
		!mDesync.Found(),
	)

	// API-03: Must-be-able-to-fail: CheckAtRest and MatchStep must refuse a live WAL
	// database (open connection with -wal/-shm) and a DB with an orphan -journal.
	pLive := filepath.Join(scratch, "live-wal.db")
	_ = copyBytes(baseDB, pLive)
	liveDB, err := r.Open(ctx, pLive)
	if err != nil {
		fatal("open live", err)
	}
	tx, err := liveDB.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin live", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO accounts(id, name, balance) VALUES(999999, 'live', 1)"); err != nil {
		fatal("exec live", err)
	}
	if err := tx.Commit(); err != nil {
		fatal("commit live", err)
	}
	errAtRestLive := recipe.CheckAtRest(pLive)
	_, errMatchLive := r.MatchStep(ctx, pLive)
	_ = liveDB.Close()

	pOrphan := filepath.Join(scratch, "orphan-journal.db")
	_ = copyBytes(baseDB, pOrphan)
	if err := os.WriteFile(pOrphan+"-journal", []byte("stale"), 0o644); err != nil {
		fatal("write orphan journal", err)
	}
	errAtRestOrphan := recipe.CheckAtRest(pOrphan)
	_, errMatchOrphan := r.MatchStep(ctx, pOrphan)
	held03 := errAtRestLive != nil && errMatchLive != nil && errAtRestOrphan != nil && errMatchOrphan != nil
	report(
		"API-03",
		"call CheckAtRest and MatchStep on an open WAL DB (with -wal/-shm) and a DB with an orphan -journal",
		"all four calls return errors refusing non-at-rest databases",
		fmt.Sprintf("live(atRestErr=%v,matchErr=%v) orphan(atRestErr=%v,matchErr=%v)",
			errAtRestLive != nil, errMatchLive != nil, errAtRestOrphan != nil, errMatchOrphan != nil),
		held03,
	)

	// API-04a: Corrupt and truncated recipe-cache files must be rebuilt cleanly.
	cachePath, err := r.CachePath()
	if err != nil {
		fatal("CachePath", err)
	}
	freshRefDir := filepath.Join(scratch, "fresh-ref")
	if err := os.MkdirAll(freshRefDir, 0o755); err != nil {
		fatal("mkdir fresh-ref", err)
	}
	goodHashes, err := r.ComputeStepHashes(ctx, freshRefDir)
	if err != nil {
		fatal("ComputeStepHashes", err)
	}
	corruptPayloads := []struct {
		name string
		body string
	}{
		{"bad-hex", "0 not-a-hash not-a-hash\n"},
		{"out-of-order", fmt.Sprintf("1 %s %s\n", goodHashes[1].Exact, goodHashes[1].Masked)},
		{"truncated", fmt.Sprintf("0 %s %s\n1 %s %s\n", goodHashes[0].Exact, goodHashes[0].Masked, goodHashes[1].Exact, goodHashes[1].Masked)},
	}
	allRecovered := true
	var failedCases []string
	for _, cp := range corruptPayloads {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o755); err != nil {
			fatal("mkdir cache", err)
		}
		if err := os.WriteFile(cachePath, []byte(cp.body), 0o644); err != nil {
			fatal("write corrupt cache", err)
		}
		got, err := r.StepHashes(ctx)
		if err != nil || !slices.Equal(got, goodHashes) {
			allRecovered = false
			failedCases = append(failedCases, fmt.Sprintf("%s(err=%v,eq=%v)", cp.name, err, slices.Equal(got, goodHashes)))
		}
	}
	report(
		"API-04a",
		"write malformed (bad-hex, out-of-order, truncated) files to recipe-cache and call recipe.StepHashes",
		"StepHashes discards corrupt/short cache, rebuilds, and returns canonical hashes",
		fmt.Sprintf("recovered=%v failures=%v", allRecovered, failedCases),
		allRecovered,
	)

	// API-04b: Syntactically valid but poisoned cache file (all-zero SHA-256 hashes).
	// Tests whether recipe-cache has any integrity check beyond line syntax.
	var poisoned strings.Builder
	zeroHash := strings.Repeat("0", 64)
	for k := 0; k <= r.Steps; k++ {
		fmt.Fprintf(&poisoned, "%d %s %s\n", k, zeroHash, zeroHash)
	}
	if err := os.WriteFile(cachePath, []byte(poisoned.String()), 0o644); err != nil {
		fatal("write poisoned cache", err)
	}
	gotPoisoned, err := r.StepHashes(ctx)
	if err != nil {
		fatal("StepHashes poisoned", err)
	}
	rejectedPoison := slices.Equal(gotPoisoned, goodHashes)
	report(
		"API-04b",
		"write syntactically valid all-zero SHA-256 hashes to recipe-cache and call recipe.StepHashes",
		"StepHashes detects tampered cache rather than returning bogus all-zero hashes",
		fmt.Sprintf("returnedCanonical=%v step0=%s", rejectedPoison, gotPoisoned[0].Exact[:16]),
		rejectedPoison,
	)
	// Clean the poisoned cache before subsequent checks.
	_ = os.Remove(cachePath)

	// API-05: Failed recipe.ApplyStep (PK conflict on re-applying step 1) must roll back
	// cleanly in both WAL and DELETE modes, leaving CurrentStep=1 and exact bytes unchanged.
	var rbIssues []string
	for _, mode := range []string{sqlitex.JournalWAL, sqlitex.JournalDelete} {
		rm := recipe.Default().WithSteps(2).WithJournalMode(mode)
		p := filepath.Join(scratch, "rollback-"+mode+".db")
		if err := rm.Build(ctx, p, 1); err != nil {
			fatal("build rb "+mode, err)
		}
		beforeHash, err := recipe.HashFile(p)
		if err != nil {
			fatal("hash before rb", err)
		}
		db, err := rm.Open(ctx, p)
		if err != nil {
			fatal("open rb", err)
		}
		applyErr := rm.ApplyStep(ctx, db, 1) // duplicate PKs from step 1
		cur, curErr := recipe.CurrentStep(ctx, db)
		closeErr := db.Close()
		if applyErr == nil {
			rbIssues = append(rbIssues, mode+":ApplyStep-succeeded-unexpectedly")
		}
		if curErr != nil || cur != 1 {
			rbIssues = append(rbIssues, fmt.Sprintf("%s:CurrentStep=%d(err=%v)", mode, cur, curErr))
		}
		if closeErr != nil {
			rbIssues = append(rbIssues, fmt.Sprintf("%s:closeErr=%v", mode, closeErr))
		}
		afterHash, err := recipe.HashFile(p)
		if err != nil {
			fatal("hash after rb", err)
		}
		if afterHash != beforeHash {
			rbIssues = append(rbIssues, fmt.Sprintf("%s:fileChanged(exact=%v,masked=%v)", mode, afterHash.Exact == beforeHash.Exact, afterHash.Masked == beforeHash.Masked))
		}
	}
	report(
		"API-05",
		"re-apply step 1 (triggering PK constraint failure) in WAL and DELETE modes, then close",
		"ApplyStep fails, CurrentStep stays 1, and closed DB file stays byte-identical",
		fmt.Sprintf("issues=%v", rbIssues),
		len(rbIssues) == 0,
	)

	// API-06: Must-be-able-to-fail: sqlitex.Open rejects mismatched file settings on existing DB.
	badOpens := []struct {
		name string
		mod  func(*sqlitex.Options)
	}{
		{"page_size=8192", func(o *sqlitex.Options) { o.File.PageSize = 8192 }},
		{"auto_vacuum=FULL", func(o *sqlitex.Options) { o.File.AutoVacuum = sqlitex.AutoVacuumFull }},
		{"application_id=0x12345678", func(o *sqlitex.Options) { o.File.ApplicationID = 0x12345678 }},
	}
	var openBypassed []string
	for _, bo := range badOpens {
		opts := r.SQLiteOptions()
		bo.mod(&opts)
		db, err := sqlitex.Open(ctx, baseDB, opts)
		if err == nil {
			db.Close()
			openBypassed = append(openBypassed, bo.name)
		}
	}
	report(
		"API-06",
		"open existing recipe DB via sqlitex.Open with mismatched page_size, auto_vacuum, and application_id",
		"sqlitex.Open rejects all three mismatched file settings with errors",
		fmt.Sprintf("bypassed=%v", openBypassed),
		len(openBypassed) == 0,
	)

	// API-07: recipe.CurrentStep on empty DB (-1), partial schema (2 of 3 tables -> error),
	// and ApplyNext past the final step -> error.
	pEmpty := filepath.Join(scratch, "empty.db")
	dbEmpty, err := r.Open(ctx, pEmpty)
	if err != nil {
		fatal("open empty", err)
	}
	stepEmpty, errEmpty := recipe.CurrentStep(ctx, dbEmpty)
	_, errCreate1 := dbEmpty.ExecContext(ctx, recipe.SchemaSQL[0])
	_, errCreate2 := dbEmpty.ExecContext(ctx, recipe.SchemaSQL[1])
	if errCreate1 != nil || errCreate2 != nil {
		fatal("create partial", fmt.Errorf("%v %v", errCreate1, errCreate2))
	}
	_, errPartial := recipe.CurrentStep(ctx, dbEmpty)
	_ = dbEmpty.Close()

	dbEnd, err := r.Open(ctx, baseDB)
	if err != nil {
		fatal("open end", err)
	}
	_, errPastEnd := r.ApplyNext(ctx, dbEnd)
	_ = dbEnd.Close()

	held07 := errEmpty == nil && stepEmpty == -1 && errPartial != nil && errPastEnd != nil
	report(
		"API-07",
		"CurrentStep on empty DB and partial schema (2/3 tables), plus ApplyNext past last step",
		"empty=-1, partial schema errors, ApplyNext past last step errors",
		fmt.Sprintf("empty=%d(err=%v) partialErr=%v pastEndErr=%v", stepEmpty, errEmpty, errPartial != nil, errPastEnd != nil),
		held07,
	)

	// API-08: Validate bounds and Step(k) independence from r.Steps.
	badRecipes := []recipe.Recipe{
		{Seed: 1, Steps: -1, JournalMode: sqlitex.JournalWAL},
		{Seed: 1, Steps: recipe.MaxSteps + 1, JournalMode: sqlitex.JournalWAL},
		{Seed: 1, Steps: 5, JournalMode: sqlitex.JournalMemory},
	}
	allInvalidRejected := true
	for _, br := range badRecipes {
		if br.Validate() == nil {
			allInvalidRejected = false
		}
	}
	sShort, err1 := recipe.Default().WithSteps(8).Step(8)
	sLong, err2 := recipe.Default().WithSteps(30).Step(8)
	stepIndep := err1 == nil && err2 == nil && reflect.DeepEqual(sShort, sLong)
	report(
		"API-08",
		"Recipe.Validate on out-of-range steps/mode, and Step(8) equality between 8-step and 30-step recipes",
		"invalid recipes rejected and Step(8) completely independent of r.Steps",
		fmt.Sprintf("invalidRejected=%v step8Equal=%v", allInvalidRejected, stepIndep),
		allInvalidRejected && stepIndep,
	)

	// API-09: Step(0) and Build(ctx, path, 0) on an invalid Recipe (Steps > MaxSteps).
	// Validate() rejects Steps=MaxSteps+1, so Step/Build should not silently accept it.
	invalidRecipe := recipe.Recipe{Seed: 1, Steps: recipe.MaxSteps + 1, JournalMode: sqlitex.JournalWAL}
	_, errStep := invalidRecipe.Step(0)
	errBuild := invalidRecipe.Build(ctx, filepath.Join(scratch, "invalid-steps.db"), 0)
	report(
		"API-09",
		"call Step(0) and Build(..., 0) on invalid Recipe with Steps=MaxSteps+1",
		"both Step(0) and Build(..., 0) reject an invalid Recipe via Validate()",
		fmt.Sprintf("step0Err=%v build0Err=%v", errStep != nil, errBuild != nil),
		errStep != nil && errBuild != nil,
	)
}
