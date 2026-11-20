package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickstrad/sqlite-internals/internal/capture"
	"github.com/nickstrad/sqlite-internals/internal/equal"
	"github.com/nickstrad/sqlite-internals/internal/recipe"
	"github.com/nickstrad/sqlite-internals/internal/sqlitex"
	"github.com/nickstrad/sqlite-internals/internal/trace"
	_ "modernc.org/sqlite"
)

const (
	childHoldTruncatedWAL = "child-hold-truncated-wal"
	childLeaveWAL         = "child-leave-wal"
	childLeaveHotJournal  = "child-leave-hot-journal"
	childMutateOnSignal   = "child-mutate-on-signal"
)

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

func openRaw(path string, readOnly bool) (*sql.DB, error) {
	q := url.Values{}
	if readOnly {
		q.Set("mode", "ro")
	}
	uri, err := sqlitex.FileURI(path, q)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func integrityCheck(ctx context.Context, path string, readOnly bool) (string, error) {
	db, err := openRaw(path, readOnly)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var res string
	err = db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&res)
	return res, err
}

func runChildMode(args ...string) {
	ctx := context.Background()
	mode, dbPath := args[0], args[1]
	switch mode {
	case childHoldTruncatedWAL:
		db, err := recipe.Default().Open(ctx, dbPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// Make one write, then checkpoint(TRUNCATE) so -wal becomes 0 bytes while -shm stays 32768 bytes.
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1 WHERE id = 1"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		var b, l, c int
		if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&b, &l, &c); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		// Start an active uncommitted write transaction and hold it open until parent closes stdin.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance + 99 WHERE id = 1"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("READY")
		buf := make([]byte, 16)
		_, _ = os.Stdin.Read(buf)
		_ = tx.Rollback()
		_ = db.Close()

	case childLeaveWAL:
		db, err := recipe.Default().Open(ctx, dbPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if _, err := db.ExecContext(ctx, "UPDATE accounts SET balance = balance + 100 WHERE id = 1"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if len(args) >= 3 && args[2] == "passive" {
			var b, l, c int
			_ = db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)").Scan(&b, &l, &c)
		}
		os.Exit(3)

	case childLeaveHotJournal:
		jmode := args[2]
		db, err := openRaw(dbPath, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA journal_mode=%s; PRAGMA cache_size=10; PRAGMA synchronous=FULL;", jmode)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err == nil {
			_, err = tx.ExecContext(ctx, "UPDATE documents SET body = zeroblob(length(body))")
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(3)

	case childMutateOnSignal:
		// Wait for a line on stdin, wait 1ms, then commit one WAL update and keep connection open until EOF.
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() {
			os.Exit(0)
		}
		time.Sleep(1 * time.Millisecond)
		db, err := recipe.Default().Open(ctx, dbPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_, _ = db.ExecContext(ctx, "UPDATE accounts SET balance = balance + 1 WHERE id = 1")
		fmt.Println("WROTE")
		for sc.Scan() {
		}
		_ = db.Close()
	}
}

func runChildExit(mode string, extra ...string) int {
	all := append([]string{mode}, extra...)
	cmdArgs, err := trace.SelfCommand(all...)
	if err != nil {
		fatal("self command", err)
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	if err != nil {
		fatal("run child "+mode, err)
	}
	return 0
}

func main() {
	if len(os.Args) >= 3 && (os.Args[1] == childHoldTruncatedWAL || os.Args[1] == childLeaveWAL || os.Args[1] == childLeaveHotJournal || os.Args[1] == childMutateOnSignal) {
		runChildMode(os.Args[1:]...)
		return
	}

	ctx := context.Background()
	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("repo root", err)
	}
	scratch := filepath.Join(repoRoot, "_work", "adv-ch02", "manifest-capture")
	if err := os.RemoveAll(scratch); err != nil {
		fatal("clean scratch", err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		fatal("mkdir scratch", err)
	}

	// Build a step-6 R0 DB (60 pages) for fast capture/tamper tests.
	r6Path := filepath.Join(scratch, "r6.db")
	if err := recipe.Default().Build(ctx, r6Path, 6); err != nil {
		fatal("build r6", err)
	}
	baseCap := filepath.Join(scratch, "base-cap")
	baseMan, err := capture.Pages(r6Path, baseCap)
	if err != nil {
		fatal("capture r6", err)
	}
	origManBytes, err := os.ReadFile(filepath.Join(baseCap, capture.ManifestFile))
	if err != nil {
		fatal("read manifest", err)
	}
	origPagesBytes, err := os.ReadFile(filepath.Join(baseCap, capture.PagesFile))
	if err != nil {
		fatal("read pages", err)
	}
	nPages := baseMan.PageCount()

	// CAP-01: Drop, duplicate, reorder, and corrupt pages across first/middle/last positions.
	// Confirm verifier refuses, names the exact page, does not clobber pre-existing output, leaves no .rebuild-tmp.
	{
		type tamperCase struct {
			name        string
			wantSubstr  string
			editEntries func([]string) []string
			editPages   func([]byte) []byte
			fixCount    bool
		}
		cases := []tamperCase{
			{
				name:       "drop-page-1",
				wantSubstr: "page 1",
				editEntries: func(e []string) []string {
					return e[1:]
				},
				fixCount: true,
			},
			{
				name:       "drop-page-25",
				wantSubstr: "page 25",
				editEntries: func(e []string) []string {
					return append(e[:24:24], e[25:]...)
				},
				fixCount: true,
			},
			{
				name:       "drop-last-manifest-only",
				wantSubstr: fmt.Sprintf("%d", nPages-1),
				editEntries: func(e []string) []string {
					return e[:len(e)-1]
				},
				fixCount: true,
			},
			{
				name:       "dup-page-1",
				wantSubstr: "page 1",
				editEntries: func(e []string) []string {
					e[1] = e[0]
					return e
				},
				fixCount: true,
			},
			{
				name:       "dup-page-25",
				wantSubstr: "page 25",
				editEntries: func(e []string) []string {
					e[25] = e[24]
					return e
				},
				fixCount: true,
			},
			{
				name:       "dup-last-page",
				wantSubstr: fmt.Sprintf("page %d", nPages),
				editEntries: func(e []string) []string {
					return append(e, e[len(e)-1])
				},
				fixCount: true,
			},
			{
				name:       "swap-adjacent-10-11",
				wantSubstr: "page 11",
				editEntries: func(e []string) []string {
					e[9], e[10] = e[10], e[9]
					return e
				},
				fixCount: true,
			},
			{
				name:       "swap-distant-5-50",
				wantSubstr: "page 50",
				editEntries: func(e []string) []string {
					e[4], e[49] = e[49], e[4]
					return e
				},
				fixCount: true,
			},
			{
				name:       "reverse-all",
				wantSubstr: fmt.Sprintf("page %d", nPages),
				editEntries: func(e []string) []string {
					slices.Reverse(e)
					return e
				},
				fixCount: true,
			},
			{
				name:       "rotate-by-1",
				wantSubstr: "page 2",
				editEntries: func(e []string) []string {
					return append(e[1:], e[0])
				},
				fixCount: true,
			},
			{
				name:       "bitflip-page-1",
				wantSubstr: "page 1",
				editPages: func(p []byte) []byte {
					p[50] ^= 0x01
					return p
				},
			},
			{
				name:       "bitflip-page-25",
				wantSubstr: "page 25",
				editPages: func(p []byte) []byte {
					p[24*4096+123] ^= 0x80
					return p
				},
			},
			{
				name:       "bitflip-last-page",
				wantSubstr: fmt.Sprintf("page %d", nPages),
				editPages: func(p []byte) []byte {
					p[(nPages-1)*4096+4095] ^= 0x01
					return p
				},
			},
		}

		var failed []string
		for _, tc := range cases {
			cdir := filepath.Join(scratch, "cap01-"+tc.name)
			if err := os.MkdirAll(cdir, 0o755); err != nil {
				fatal("mkdir cap01", err)
			}
			mText := string(origManBytes)
			pBytes := append([]byte(nil), origPagesBytes...)
			if tc.editEntries != nil {
				lines := strings.Split(strings.TrimSuffix(mText, "\n"), "\n")
				entries := tc.editEntries(append([]string(nil), lines[3:]...))
				lines = append(lines[:3:3], entries...)
				if tc.fixCount {
					lines[2] = fmt.Sprintf("pages %d", len(entries))
				}
				mText = strings.Join(lines, "\n") + "\n"
			}
			if tc.editPages != nil {
				pBytes = tc.editPages(pBytes)
			}
			if err := os.WriteFile(filepath.Join(cdir, capture.ManifestFile), []byte(mText), 0o644); err != nil {
				fatal("write man", err)
			}
			if err := os.WriteFile(filepath.Join(cdir, capture.PagesFile), pBytes, 0o644); err != nil {
				fatal("write pages", err)
			}
			// Pre-populate outPath with sentinel bytes to confirm Rebuild never clobbers an existing file on error.
			outPath := filepath.Join(cdir, "existing.db")
			sentinel := []byte("DO-NOT-CLOBBER")
			if err := os.WriteFile(outPath, sentinel, 0o644); err != nil {
				fatal("write sentinel", err)
			}
			_, err := capture.Rebuild(cdir, outPath)
			afterSentinel, _ := os.ReadFile(outPath)
			_, tmpErr := os.Stat(outPath + ".rebuild-tmp")
			if err == nil {
				failed = append(failed, tc.name+":accepted")
			} else if !strings.Contains(err.Error(), tc.wantSubstr) {
				failed = append(failed, fmt.Sprintf("%s:err=%q(want %q)", tc.name, err.Error(), tc.wantSubstr))
			} else if string(afterSentinel) != string(sentinel) {
				failed = append(failed, tc.name+":clobbered-existing")
			} else if !errors.Is(tmpErr, os.ErrNotExist) {
				failed = append(failed, tc.name+":left-tmp")
			}
		}
		report("CAP-01",
			fmt.Sprintf("tamper capture in %d ways (drop/dup/swap/reverse/rotate/bitflip at first/middle/last) with pre-existing outPath", len(cases)),
			"every tamper is refused naming the page, existing outPath is untouched, no .rebuild-tmp left",
			fmt.Sprintf("tested=%d, failed=%d (%v)", len(cases), len(failed), failed),
			len(failed) == 0)
	}

	// CAP-02a: Coordinated drop of the last page from BOTH manifest.txt (pages N-1) AND pages.bin.
	{
		cdir := filepath.Join(scratch, "cap02a-coord-drop-last")
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			fatal("mkdir cap02a", err)
		}
		lines := strings.Split(strings.TrimSuffix(string(origManBytes), "\n"), "\n")
		entries := lines[3 : len(lines)-1]
		lines = append(lines[:3:3], entries...)
		lines[2] = fmt.Sprintf("pages %d", len(entries))
		if err := os.WriteFile(filepath.Join(cdir, capture.ManifestFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			fatal("write man 02a", err)
		}
		if err := os.WriteFile(filepath.Join(cdir, capture.PagesFile), origPagesBytes[:len(entries)*4096], 0o644); err != nil {
			fatal("write pages 02a", err)
		}
		out := filepath.Join(cdir, "rebuilt.db")
		_, rebuildErr := capture.Rebuild(cdir, out)
		var sqlIC string
		if rebuildErr == nil {
			ic, icErr := integrityCheck(ctx, out, true)
			if icErr != nil {
				sqlIC = "err: " + icErr.Error()
			} else {
				sqlIC = ic
			}
		}
		report("CAP-02a",
			"drop last page from BOTH manifest.txt (updating pages N-1) and pages.bin, then call capture.Rebuild",
			"Rebuild rejects the capture because page 1 header declares N pages while capture has N-1",
			fmt.Sprintf("rebuild_err=%v, sqlite_on_rebuilt=%q", rebuildErr, sqlIC),
			rebuildErr != nil)
	}

	// CAP-02b: Tamper with manifest.txt role column (unknown role 'bogus-role' and wrong role 'overflow' for page 1).
	{
		var accepted []string
		for _, newRole := range []string{"bogus-role", "overflow"} {
			cdir := filepath.Join(scratch, "cap02b-"+newRole)
			if err := os.MkdirAll(cdir, 0o755); err != nil {
				fatal("mkdir cap02b", err)
			}
			lines := strings.Split(strings.TrimSuffix(string(origManBytes), "\n"), "\n")
			fields := strings.Fields(lines[3])
			fields[1] = newRole
			lines[3] = strings.Join(fields, " ")
			if err := os.WriteFile(filepath.Join(cdir, capture.ManifestFile), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
				fatal("write man 02b", err)
			}
			if err := os.WriteFile(filepath.Join(cdir, capture.PagesFile), origPagesBytes, 0o644); err != nil {
				fatal("write pages 02b", err)
			}
			out := filepath.Join(cdir, "rebuilt.db")
			if _, err := capture.Rebuild(cdir, out); err == nil {
				accepted = append(accepted, newRole)
			}
		}
		report("CAP-02b",
			"replace page 1's role in manifest.txt with 'bogus-role' and with 'overflow', keeping sha256 valid",
			"Rebuild rejects unknown or mismatched page roles in the manifest",
			fmt.Sprintf("accepted_tampered_roles=%v", accepted),
			len(accepted) == 0)
	}

	// CAP-02c: Empty 0-page manifest (pages 0 + 0-byte pages.bin).
	{
		cdir := filepath.Join(scratch, "cap02c-zero-pages")
		if err := os.MkdirAll(cdir, 0o755); err != nil {
			fatal("mkdir cap02c", err)
		}
		manText := equal.ManifestFormat + "\npage-size 4096\npages 0\n"
		if err := os.WriteFile(filepath.Join(cdir, capture.ManifestFile), []byte(manText), 0o644); err != nil {
			fatal("write man 02c", err)
		}
		if err := os.WriteFile(filepath.Join(cdir, capture.PagesFile), nil, 0o644); err != nil {
			fatal("write pages 02c", err)
		}
		out := filepath.Join(cdir, "rebuilt.db")
		_, rebuildErr := capture.Rebuild(cdir, out)
		var outSize int64 = -1
		if info, err := os.Stat(out); err == nil {
			outSize = info.Size()
		}
		report("CAP-02c",
			"feed a 0-page manifest (pages 0 + empty pages.bin) to capture.Rebuild",
			"Rebuild rejects a 0-page manifest rather than writing an empty 0-byte file",
			fmt.Sprintf("rebuild_err=%v, out_size=%d", rebuildErr, outSize),
			rebuildErr != nil)
	}

	// CAP-03: Not-at-rest refusals across WAL (uncheckpointed + passive-checkpointed),
	// hot journals in DELETE/TRUNCATE/PERSIST modes, and stale side files next to Rebuild outPath.
	{
		var failed []string
		// (a) WAL with uncheckpointed frames
		pWal := filepath.Join(scratch, "nar-wal.db")
		if err := recipe.Default().Build(ctx, pWal, 4); err != nil {
			fatal("build nar-wal", err)
		}
		_ = runChildExit(childLeaveWAL, pWal)
		if _, err := capture.Pages(pWal, filepath.Join(scratch, "nar-wal-cap")); !errors.Is(err, capture.ErrNotAtRest) {
			failed = append(failed, fmt.Sprintf("wal-unckpt:err=%v", err))
		}
		// (b) WAL after PASSIVE checkpoint (non-empty -wal still on disk)
		pPass := filepath.Join(scratch, "nar-wal-passive.db")
		if err := recipe.Default().Build(ctx, pPass, 4); err != nil {
			fatal("build nar-wal-passive", err)
		}
		_ = runChildExit(childLeaveWAL, pPass, "passive")
		if _, err := capture.Pages(pPass, filepath.Join(scratch, "nar-wal-passive-cap")); !errors.Is(err, capture.ErrNotAtRest) {
			failed = append(failed, fmt.Sprintf("wal-passive:err=%v", err))
		}
		// (c) Hot journal in DELETE, TRUNCATE, PERSIST modes
		for _, jm := range []string{"delete", "truncate", "persist"} {
			pJ := filepath.Join(scratch, "nar-j-"+jm+".db")
			if err := recipe.Default().WithJournalMode("delete").Build(ctx, pJ, 6); err != nil {
				fatal("build nar-j-"+jm, err)
			}
			_ = runChildExit(childLeaveHotJournal, pJ, jm)
			if _, err := capture.Pages(pJ, filepath.Join(scratch, "nar-j-cap-"+jm)); !errors.Is(err, capture.ErrNotAtRest) {
				failed = append(failed, fmt.Sprintf("hot-journal-%s:err=%v", jm, err))
			}
		}
		// (d) Stale side files next to Rebuild outPath
		for _, suf := range []string{"-wal", "-journal", "-shm"} {
			out := filepath.Join(scratch, "stale-out"+suf+".db")
			if err := os.WriteFile(out+suf, []byte("x"), 0o644); err != nil {
				fatal("write stale side", err)
			}
			if _, err := capture.Rebuild(baseCap, out); err == nil {
				failed = append(failed, "rebuild-ignored-stale"+suf)
			}
		}
		report("CAP-03",
			"not-at-rest refusals: uncheckpointed -wal, passive-ckpt -wal, hot -journal (delete/truncate/persist), and stale Rebuild side files",
			"all 8 non-at-rest conditions are refused",
			fmt.Sprintf("checked=8, failed=%d (%v)", len(failed), failed),
			len(failed) == 0)
	}

	// CAP-04: Live OPEN WAL connection holding an active uncommitted transaction AFTER wal_checkpoint(TRUNCATE).
	// Under the chapter's "at rest" rule (file alone is complete: 0-byte -wal and no hot -journal),
	// capture.Pages accepts a 0-byte -wal and must produce a valid rebuilt snapshot of the committed
	// state (including the checkpointed +1, excluding the uncommitted +99, E1-equal after rollback).
	{
		pLive := filepath.Join(scratch, "live-trunc-wal.db")
		if err := recipe.Default().Build(ctx, pLive, 4); err != nil {
			fatal("build live-trunc-wal", err)
		}
		queryBal := func(path string) int {
			db, err := openRaw(path, true)
			if err != nil {
				fatal("open bal", err)
			}
			defer db.Close()
			var b int
			if err := db.QueryRowContext(ctx, "SELECT balance FROM accounts WHERE id = 1").Scan(&b); err != nil {
				fatal("query bal", err)
			}
			return b
		}
		balBefore := queryBal(pLive)

		cmdArgs, err := trace.SelfCommand(childHoldTruncatedWAL, pLive)
		if err != nil {
			fatal("self cmd hold", err)
		}
		cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			fatal("stdin pipe", err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			fatal("stdout pipe", err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			fatal("start hold child", err)
		}
		sc := bufio.NewScanner(stdout)
		if !sc.Scan() || strings.TrimSpace(sc.Text()) != "READY" {
			_ = cmd.Process.Kill()
			fatal("wait READY", fmt.Errorf("got %q", sc.Text()))
		}
		walInfo, walErr := os.Stat(pLive + "-wal")
		shmInfo, shmErr := os.Stat(pLive + "-shm")
		walSz, shmSz := int64(-1), int64(-1)
		if walErr == nil {
			walSz = walInfo.Size()
		}
		if shmErr == nil {
			shmSz = shmInfo.Size()
		}

		// While the child holds an uncommitted +99 write tx after checkpoint(TRUNCATE),
		// capture.Pages accepts the 0-byte -wal and must rebuild the exact committed state.
		capDir := filepath.Join(scratch, "live-trunc-cap")
		atRestErr := capture.CheckAtRest(pLive)
		m, capErr := capture.Pages(pLive, capDir)

		_ = stdin.Close()
		_ = cmd.Wait()

		rebuilt := filepath.Join(scratch, "live-trunc-rebuilt.db")
		var rebuildErr error
		var rebuiltIC string
		var rebuiltBal int
		var cmp equal.Result
		if capErr == nil {
			_, rebuildErr = capture.Rebuild(capDir, rebuilt)
			if rebuildErr == nil {
				rebuiltIC, _ = integrityCheck(ctx, rebuilt, true)
				rebuiltBal = queryBal(rebuilt)
				cmp, _ = equal.CompareFiles(pLive, rebuilt, m.PageSize)
			}
		}

		held := atRestErr == nil && capErr == nil && rebuildErr == nil &&
			rebuiltIC == "ok" && rebuiltBal == balBefore+1 && cmp.Equal
		report("CAP-04",
			"capture a WAL DB while a live connection holds an uncommitted +99 write tx after wal_checkpoint(TRUNCATE) (-wal=0 bytes, -shm=32768 bytes)",
			fmt.Sprintf("0-byte -wal is at rest: capture rebuilds a valid DB with committed balance %d (excluding uncommitted +99) and E1-equal after rollback", balBefore+1),
			fmt.Sprintf("-wal=%d bytes, -shm=%d bytes, CheckAtRest=%v, capErr=%v, rebuildErr=%v, ic=%q, bal=%d (want %d), e1=%v",
				walSz, shmSz, atRestErr, capErr, rebuildErr, rebuiltIC, rebuiltBal, balBefore+1, cmp.Equal),
			held)
	}

	// CAP-05: Mid-capture mutation hygiene — if capture.Pages fails because the DB changed during capture,
	// does it clean up pages.bin and manifest.txt or leave a usable capture directory behind?
	{
		// Build a 30-step DB (~1.1 MB, 270 pages) so capture.Pages takes a few ms, making mid-capture races easy to trigger.
		pRace := filepath.Join(scratch, "race-r30.db")
		if err := recipe.Default().Build(ctx, pRace, 30); err != nil {
			fatal("build race-r30", err)
		}
		var triggeredErr error
		var leftPages, leftManifest, rebuildSucceeded bool
		var attempts int
		for attempt := 1; attempt <= 40; attempt++ {
			attempts = attempt
			// Ensure clean at-rest DB copy for this attempt.
			workDB := filepath.Join(scratch, fmt.Sprintf("race-%d.db", attempt))
			raw, _ := os.ReadFile(pRace)
			if err := os.WriteFile(workDB, raw, 0o644); err != nil {
				fatal("write race db", err)
			}
			cmdArgs, err := trace.SelfCommand(childMutateOnSignal, workDB)
			if err != nil {
				fatal("self cmd mutate", err)
			}
			cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
			stdin, _ := cmd.StdinPipe()
			stdout, _ := cmd.StdoutPipe()
			if err := cmd.Start(); err != nil {
				fatal("start mutate", err)
			}
			// Signal child to commit after 1ms, and immediately start capture.Pages.
			_, _ = stdin.Write([]byte("GO\n"))
			capDir := filepath.Join(scratch, fmt.Sprintf("race-cap-%d", attempt))
			_, capErr := capture.Pages(workDB, capDir)

			// Wait for child to finish its write and exit cleanly.
			sc := bufio.NewScanner(stdout)
			sc.Scan()
			_ = stdin.Close()
			_ = cmd.Wait()

			if capErr != nil && strings.Contains(capErr.Error(), "changed during the capture") {
				triggeredErr = capErr
				_, errP := os.Stat(filepath.Join(capDir, capture.PagesFile))
				_, errM := os.Stat(filepath.Join(capDir, capture.ManifestFile))
				leftPages = errP == nil
				leftManifest = errM == nil
				if leftPages && leftManifest {
					_, rErr := capture.Rebuild(capDir, filepath.Join(scratch, fmt.Sprintf("race-rebuilt-%d.db", attempt)))
					rebuildSucceeded = rErr == nil
				}
				break
			}
		}
		if triggeredErr == nil {
			report("CAP-05",
				fmt.Sprintf("trigger mid-capture WAL write across %d attempts to inspect failure cleanup", attempts),
				"mid-capture failure cleans up pages.bin and manifest.txt",
				"could not hit the timing window within 40 attempts (race not triggered)",
				true)
		} else {
			clean := !leftPages && !leftManifest
			report("CAP-05",
				fmt.Sprintf("trigger mid-capture WAL write during capture.Pages (hit on attempt %d)", attempts),
				"failed capture.Pages removes partial/stale pages.bin and manifest.txt from target dir",
				fmt.Sprintf("err=%q, left_pages_bin=%v, left_manifest_txt=%v, Rebuild_accepts_failed_dir=%v",
					triggeredErr.Error(), leftPages, leftManifest, rebuildSucceeded),
				clean)
		}
	}

	// CAP-06: At-rest exceptions & cross-config R1 capture/rebuild parity (TRUNCATE 0-byte journal,
	// PERSIST zeroed journal, 0-byte -wal, page sizes 512 & 65536, auto_vacuum=FULL, UTF-16be, and trailing slack refusal).
	{
		var issues []string
		checkRoundTrip := func(label, dbPath string) {
			cdir := filepath.Join(scratch, "rt-cap-"+label)
			m, err := capture.Pages(dbPath, cdir)
			if err != nil {
				issues = append(issues, fmt.Sprintf("%s:capture(%v)", label, err))
				return
			}
			out := filepath.Join(scratch, "rt-rebuilt-"+label+".db")
			if _, err := capture.Rebuild(cdir, out); err != nil {
				issues = append(issues, fmt.Sprintf("%s:rebuild(%v)", label, err))
				return
			}
			cmp, err := equal.CompareFiles(dbPath, out, m.PageSize)
			if err != nil || !cmp.Equal {
				issues = append(issues, fmt.Sprintf("%s:compare(%v,%v)", label, cmp, err))
				return
			}
			ic, err := integrityCheck(ctx, out, true)
			if err != nil || ic != "ok" {
				issues = append(issues, fmt.Sprintf("%s:ic(%q,%v)", label, ic, err))
			}
		}

		// 1. TRUNCATE mode closed (leaves 0-byte -journal)
		pTrunc := filepath.Join(scratch, "atrest-truncate.db")
		if err := recipe.Default().WithJournalMode("delete").Build(ctx, pTrunc, 3); err != nil {
			fatal("build truncate", err)
		}
		if db, err := openRaw(pTrunc, false); err != nil {
			fatal("open truncate", err)
		} else {
			if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=TRUNCATE; UPDATE accounts SET balance = balance + 1 WHERE id = 1;"); err != nil {
				fatal("exec truncate", err)
			}
			db.Close()
		}
		checkRoundTrip("truncate-0b-journal", pTrunc)

		// 2. PERSIST mode closed (leaves zeroed-header -journal)
		pPers := filepath.Join(scratch, "atrest-persist.db")
		if err := recipe.Default().WithJournalMode("delete").Build(ctx, pPers, 3); err != nil {
			fatal("build persist", err)
		}
		if db, err := openRaw(pPers, false); err != nil {
			fatal("open persist", err)
		} else {
			if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=PERSIST; UPDATE accounts SET balance = balance + 1 WHERE id = 1;"); err != nil {
				fatal("exec persist", err)
			}
			db.Close()
		}
		checkRoundTrip("persist-zero-journal", pPers)

		// 3. Closed DB with 0-byte -wal present
		pZeroWal := filepath.Join(scratch, "atrest-0b-wal.db")
		if err := recipe.Default().Build(ctx, pZeroWal, 3); err != nil {
			fatal("build 0b-wal", err)
		}
		if err := os.WriteFile(pZeroWal+"-wal", nil, 0o644); err != nil {
			fatal("write 0b-wal", err)
		}
		checkRoundTrip("zero-byte-wal", pZeroWal)

		// 4. Page sizes 512 & 65536, auto_vacuum=FULL, UTF-16be
		configs := []struct {
			label string
			edit  func(*sqlitex.Options)
		}{
			{"ps512", func(o *sqlitex.Options) { o.File.PageSize = 512 }},
			{"ps65536", func(o *sqlitex.Options) { o.File.PageSize = 65536 }},
			{"autovac-full", func(o *sqlitex.Options) { o.File.AutoVacuum = sqlitex.AutoVacuumFull }},
			{"utf16be", func(o *sqlitex.Options) { o.File.Encoding = "UTF-16be" }},
		}
		for _, cfg := range configs {
			p := filepath.Join(scratch, "cfg-"+cfg.label+".db")
			opts := sqlitex.DefaultOptions()
			opts.SingleConnection = true
			opts.Conn.SecureDelete = true
			cfg.edit(&opts)
			db, err := sqlitex.Open(ctx, p, opts)
			if err != nil {
				fatal("open "+cfg.label, err)
			}
			if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, b BLOB); INSERT INTO t VALUES(1, zeroblob(5000));"); err != nil {
				fatal("exec "+cfg.label, err)
			}
			db.Close()
			checkRoundTrip(cfg.label, p)
		}

		// 5. Trailing slack page (in-header size = filePages - 1, counters agree): capture.Pages must refuse!
		pSlack := filepath.Join(scratch, "slack.db")
		raw, _ := os.ReadFile(pTrunc)
		nSlack := uint32(len(raw)/4096) - 1
		binary.BigEndian.PutUint32(raw[28:32], nSlack)
		if err := os.WriteFile(pSlack, raw, 0o644); err != nil {
			fatal("write slack", err)
		}
		if _, err := capture.Pages(pSlack, filepath.Join(scratch, "slack-cap")); err == nil {
			issues = append(issues, "slack-page-accepted")
		}

		report("CAP-06",
			"R1 capture->rebuild E1 round-trip on 0-byte/zeroed journals, 0-byte -wal, ps 512/65536, auto_vacuum=FULL, UTF-16be + slack-page refusal",
			"all 7 valid at-rest DBs rebuild E1-identical with integrity_check=ok; slack-page DB is refused",
			fmt.Sprintf("issues=%d (%v)", len(issues), issues),
			len(issues) == 0)
	}

	// CAP-07: R1 capture of a closed, valid SQLite DB with a WITHOUT ROWID table.
	{
		pWR := filepath.Join(scratch, "cap-without-rowid.db")
		opts := sqlitex.DefaultOptions()
		opts.SingleConnection = true
		db, err := sqlitex.Open(ctx, pWR, opts)
		if err != nil {
			fatal("open cap-wr", err)
		}
		if _, err := db.ExecContext(ctx, "CREATE TABLE wr(k TEXT PRIMARY KEY, v INT) WITHOUT ROWID; INSERT INTO wr VALUES('a',1),('b',2);"); err != nil {
			fatal("exec cap-wr", err)
		}
		db.Close()
		ic, _ := integrityCheck(ctx, pWR, true)
		_, capErr := capture.Pages(pWR, filepath.Join(scratch, "cap-wr-dir"))
		report("CAP-07",
			"capture.Pages on a closed, valid SQLite database containing a WITHOUT ROWID table",
			"R1 capture succeeds on any closed valid SQLite database (integrity_check=ok)",
			fmt.Sprintf("sqlite_ic=%q, capture.Pages_err=%v", ic, capErr),
			ic == "ok" && capErr == nil)
	}

	// CAP-08: internal/equal E1 & manifest edge cases.
	{
		var issues []string
		// CompareImages with non-multiple length and pageSize <= 0
		r1 := equal.CompareImages([]byte("abc"), []byte("abcd"), 4096)
		if r1.Equal || r1.FirstDifferingPage != 1 || r1.DifferingPages != 1 {
			issues = append(issues, fmt.Sprintf("non-multiple:%+v", r1))
		}
		r2 := equal.CompareImages(nil, []byte("x"), 0)
		if r2.Equal || r2.FirstDifferingPage != 0 {
			issues = append(issues, fmt.Sprintf("ps0:%+v", r2))
		}
		// CompareManifests edge cases
		mA := equal.Manifest{
			PageSize: 4096,
			Entries: []equal.ManifestEntry{
				{Page: 1, Role: "table-leaf", SHA256: strings.Repeat("a", 64)},
				{Page: 2, Role: "table-leaf", SHA256: strings.Repeat("b", 64)},
			},
		}
		mRoleDiff := equal.Manifest{
			PageSize: 4096,
			Entries: []equal.ManifestEntry{
				{Page: 1, Role: "table-leaf", SHA256: strings.Repeat("a", 64)},
				{Page: 2, Role: "overflow", SHA256: strings.Repeat("b", 64)},
			},
		}
		dRole := equal.CompareManifests(mA, mRoleDiff)
		if dRole.Equal || dRole.FirstDifferingPage != 2 || !strings.Contains(dRole.Reason, "role") {
			issues = append(issues, fmt.Sprintf("role-diff:%+v", dRole))
		}
		// ParseManifest bad inputs
		badManifests := []struct {
			label string
			text  string
		}{
			{"uppercase-hex", equal.ManifestFormat + "\npage-size 4096\npages 1\n1 table-leaf " + strings.Repeat("A", 64) + "\n"},
			{"trailing-blank", equal.ManifestFormat + "\npage-size 4096\npages 1\n1 table-leaf " + strings.Repeat("a", 64) + "\n\n"},
			{"neg-pagesize", equal.ManifestFormat + "\npage-size -4096\npages 0\n"},
			{"extra-column", equal.ManifestFormat + "\npage-size 4096\npages 1\n1 table-leaf " + strings.Repeat("a", 64) + " extra\n"},
			{"count-mismatch", equal.ManifestFormat + "\npage-size 4096\npages 2\n1 table-leaf " + strings.Repeat("a", 64) + "\n"},
		}
		for _, bm := range badManifests {
			if _, err := equal.ParseManifest(strings.NewReader(bm.text)); err == nil {
				issues = append(issues, "ParseManifest accepted "+bm.label)
			}
		}
		report("CAP-08",
			"internal/equal CompareImages, CompareManifests, and ParseManifest boundary/rejection checks",
			"all edge cases return expected diffs and ParseManifest rejects all 5 malformed manifests",
			fmt.Sprintf("issues=%d (%v)", len(issues), issues),
			len(issues) == 0)
	}
}
