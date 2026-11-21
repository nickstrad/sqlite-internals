// Command kill-at-syscall runs adversarial tests against internal/trace's
// strace --inject SIGKILL mechanism: K beyond the syscall count, K=1 (sync and
// pwrite64), 5-run repeatability across multiple kill points, and invalid inputs.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/nickstrad/walfleet/internal/sqlitex"
	"github.com/nickstrad/walfleet/internal/trace"
)

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "|", "/")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ; ")
	return strings.TrimSpace(s)
}

func report(id, tried, expect, got string, held bool) {
	verdict := "BROKEN"
	if held {
		verdict = "HELD"
	}
	fmt.Printf("ATTACK %s | %s | EXPECT %s | GOT %s | %s\n",
		id, sanitize(tried), sanitize(expect), sanitize(got), verdict)
}

func fatal(msg string, err error) {
	fmt.Fprintf(os.Stderr, "HARNESS ERROR: %s: %v\n", msg, err)
	os.Exit(2)
}

func labOptions() sqlitex.Options {
	opts := sqlitex.DefaultOptions()
	opts.SingleConnection = true
	opts.Conn.WALAutocheckpoint = 0
	return opts
}

func createBase(ctx context.Context, dbPath string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(dbPath + suffix)
	}
	db, err := sqlitex.Open(ctx, dbPath, labOptions())
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT NOT NULL); INSERT INTO t VALUES(1, 'seed')"); err != nil {
		db.Close()
		return err
	}
	return db.Close()
}

func childWriter(dbPath string) {
	// Pin to one OS thread so strace's per-thread inject counter sees SQLite's syscalls in order.
	runtime.LockOSThread()
	ctx := context.Background()
	db, err := sqlitex.Open(ctx, dbPath, labOptions())
	if err != nil {
		fmt.Fprintln(os.Stderr, "child open:", err)
		os.Exit(2)
	}
	for _, id := range []int{2, 3} {
		if _, err := db.ExecContext(ctx, "INSERT INTO t(id, v) VALUES(?, ?)", id, fmt.Sprintf("row-%d", id)); err != nil {
			fmt.Fprintln(os.Stderr, "child insert:", err)
			os.Exit(2)
		}
		fmt.Printf("ack %d\n", id)
	}
	if err := db.Close(); err != nil {
		fmt.Fprintln(os.Stderr, "child close:", err)
		os.Exit(2)
	}
}

func inspectDB(ctx context.Context, dbPath string) (rows int, integrity string, err error) {
	db, err := sqlitex.Open(ctx, dbPath, labOptions())
	if err != nil {
		return 0, "", err
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM t").Scan(&rows); err != nil {
		return 0, "", err
	}
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return 0, "", err
	}
	return rows, integrity, nil
}

func isSync(name string) bool {
	return name == "fsync" || name == "fdatasync"
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "child-writer" {
		childWriter(os.Args[2])
		return
	}

	if !trace.Supported {
		trace.PrintLinuxOnly("kill-at-syscall adversarial attacks", "agent_work/ch00/adversarial/kill-at-syscall")
		return
	}
	if _, err := trace.StracePath(); err != nil {
		fatal("strace required", err)
	}

	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("locate repo root", err)
	}
	workDir := filepath.Join(repoRoot, "_work", "adv-ch00", "kill-at-syscall")
	if err := os.RemoveAll(workDir); err != nil {
		fatal("clean work dir", err)
	}
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		fatal("mkdir work dir", err)
	}
	realWorkDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		fatal("eval symlinks", err)
	}

	ctx := context.Background()
	dbPath := filepath.Join(realWorkDir, "kill.db")
	childCmd, err := trace.SelfCommand("child-writer", dbPath)
	if err != nil {
		fatal("self command", err)
	}

	// Measure baseline sync count for the deterministic child workload.
	if err := createBase(ctx, dbPath); err != nil {
		fatal("baseline createBase", err)
	}
	baseline, err := trace.Run(ctx, trace.Options{
		Command:  childCmd,
		Syscalls: []string{"fsync", "fdatasync", "pwrite64"},
	})
	if err != nil {
		fatal("baseline trace.Run", err)
	}
	totalSyncs := 0
	for _, c := range baseline.Log.Calls {
		if isSync(c.Name) && c.Succeeded() {
			totalSyncs++
		}
	}
	if baseline.ExitCode != 0 || totalSyncs == 0 {
		fatal(fmt.Sprintf("unexpected baseline exit=%d totalSyncs=%d", baseline.ExitCode, totalSyncs), nil)
	}

	// KILL-01: K beyond total syscall count (totalSyncs + 50).
	if err := createBase(ctx, dbPath); err != nil {
		fatal("KILL-01 createBase", err)
	}
	beyondK := totalSyncs + 50
	res01, err := trace.Run(ctx, trace.Options{
		Command:  childCmd,
		Syscalls: []string{"fsync", "fdatasync", "pwrite64"},
		KillAt:   &trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: beyondK},
	})
	if err != nil {
		fatal("KILL-01 trace.Run", err)
	}
	rows01, integ01, err := inspectDB(ctx, dbPath)
	if err != nil {
		fatal("KILL-01 inspectDB", err)
	}
	held01 := res01.ExitCode == 0 && res01.Killed == "" &&
		strings.Contains(res01.Stdout, "ack 2") && strings.Contains(res01.Stdout, "ack 3") &&
		rows01 == 3 && integ01 == "ok"
	report(
		"KILL-01",
		fmt.Sprintf("KillAt fsync,fdatasync with K=%d (baseline has %d syncs)", beyondK, totalSyncs),
		"process completes normally (exit=0, Killed empty, 3 rows, integrity=ok)",
		fmt.Sprintf("exit=%d killed=%q rows=%d integrity=%s stdout=%q", res01.ExitCode, res01.Killed, rows01, integ01, strings.TrimSpace(res01.Stdout)),
		held01,
	)

	// KILL-02: K=1 on fsync,fdatasync.
	if err := createBase(ctx, dbPath); err != nil {
		fatal("KILL-02 createBase", err)
	}
	res02, err := trace.Run(ctx, trace.Options{
		Command:  childCmd,
		Syscalls: []string{"fsync", "fdatasync", "pwrite64"},
		KillAt:   &trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: 1},
	})
	if err != nil {
		fatal("KILL-02 trace.Run", err)
	}
	var lastSync02 trace.Syscall
	completedSyncs02 := 0
	for _, c := range trace.RelativeTo(realWorkDir, res02.Log.Calls) {
		if isSync(c.Name) {
			lastSync02 = c
			if c.Succeeded() {
				completedSyncs02++
			}
		}
	}
	rows02, integ02, err := inspectDB(ctx, dbPath)
	if err != nil {
		fatal("KILL-02 inspectDB", err)
	}
	held02 := res02.Killed == "SIGKILL" && res02.ExitCode == -1 &&
		lastSync02.Unfinished && completedSyncs02 == 0 &&
		!strings.Contains(res02.Stdout, "ack") &&
		rows02 == 1 && integ02 == "ok"
	report(
		"KILL-02",
		"KillAt fsync,fdatasync with K=1",
		"SIGKILL on entry to 1st sync (exit=-1, unfinished=true, 0 completed syncs, no ack, 1 row, integrity=ok)",
		fmt.Sprintf("exit=%d killed=%q lastSync=(%s %s unfinished=%v) completedSyncs=%d stdout=%q rows=%d integrity=%s",
			res02.ExitCode, res02.Killed, lastSync02.Name, lastSync02.Path, lastSync02.Unfinished, completedSyncs02, strings.TrimSpace(res02.Stdout), rows02, integ02),
		held02,
	)

	// KILL-03: K=1 on pwrite64.
	if err := createBase(ctx, dbPath); err != nil {
		fatal("KILL-03 createBase", err)
	}
	res03, err := trace.Run(ctx, trace.Options{
		Command:  childCmd,
		Syscalls: []string{"pwrite64", "fsync", "fdatasync"},
		KillAt:   &trace.KillAt{Syscalls: []string{"pwrite64"}, When: 1},
	})
	if err != nil {
		fatal("KILL-03 trace.Run", err)
	}
	var lastPwrite03 trace.Syscall
	completedPwrites03 := 0
	for _, c := range trace.RelativeTo(realWorkDir, res03.Log.Calls) {
		if c.Name == "pwrite64" {
			lastPwrite03 = c
			if c.Succeeded() {
				completedPwrites03++
			}
		}
	}
	rows03, integ03, err := inspectDB(ctx, dbPath)
	if err != nil {
		fatal("KILL-03 inspectDB", err)
	}
	held03 := res03.Killed == "SIGKILL" && res03.ExitCode == -1 &&
		lastPwrite03.Unfinished && completedPwrites03 == 0 &&
		rows03 == 1 && integ03 == "ok"
	report(
		"KILL-03",
		"KillAt pwrite64 with K=1",
		"SIGKILL on entry to 1st pwrite64 (exit=-1, unfinished=true, 0 completed pwrites, 1 row, integrity=ok)",
		fmt.Sprintf("exit=%d killed=%q lastPwrite=(%s off=%d unfinished=%v) completed=%d rows=%d integrity=%s",
			res03.ExitCode, res03.Killed, lastPwrite03.Path, lastPwrite03.Offset, lastPwrite03.Unfinished, completedPwrites03, rows03, integ03),
		held03,
	)

	// KILL-04: repeatability over 5 runs at each of K=1, K=2, K=3 on fsync,fdatasync.
	const runsPerK = 5
	allRepeatable := true
	var summaryParts []string
	for _, k := range []int{1, 2, 3} {
		if k > totalSyncs {
			break
		}
		var baselineSig string
		uniqueSigs := map[string]int{}
		allIntegOK := true
		for run := 1; run <= runsPerK; run++ {
			if err := createBase(ctx, dbPath); err != nil {
				fatal(fmt.Sprintf("KILL-04 K=%d run=%d createBase", k, run), err)
			}
			res, err := trace.Run(ctx, trace.Options{
				Command:  childCmd,
				Syscalls: []string{"fsync", "fdatasync", "pwrite64"},
				KillAt:   &trace.KillAt{Syscalls: []string{"fsync", "fdatasync"}, When: k},
			})
			if err != nil {
				fatal(fmt.Sprintf("KILL-04 K=%d run=%d trace.Run", k, run), err)
			}
			var lastSync trace.Syscall
			completedSyncs := 0
			walWrites := 0
			for _, c := range trace.RelativeTo(realWorkDir, res.Log.Calls) {
				if isSync(c.Name) {
					lastSync = c
					if c.Succeeded() {
						completedSyncs++
					}
				}
				if c.Name == "pwrite64" && c.Path == "kill.db-wal" && c.Succeeded() {
					walWrites++
				}
			}
			rows, integ, err := inspectDB(ctx, dbPath)
			if err != nil {
				fatal(fmt.Sprintf("KILL-04 K=%d run=%d inspectDB", k, run), err)
			}
			if integ != "ok" {
				allIntegOK = false
			}
			sig := fmt.Sprintf("sig=%s/exit=%d/sync=%s(%s,unfin=%v)/doneSync=%d/walW=%d/rows=%d/ack=%q",
				res.Killed, res.ExitCode, lastSync.Name, lastSync.Path, lastSync.Unfinished,
				completedSyncs, walWrites, rows, strings.TrimSpace(res.Stdout))
			if baselineSig == "" {
				baselineSig = sig
			}
			uniqueSigs[sig]++
		}
		if len(uniqueSigs) != 1 || !allIntegOK {
			allRepeatable = false
		}
		summaryParts = append(summaryParts, fmt.Sprintf("K=%d:unique=%d/5(%s)", k, len(uniqueSigs), baselineSig))
	}
	report(
		"KILL-04",
		fmt.Sprintf("repeatability across %d runs each at K=1, K=2, K=3 on fsync,fdatasync", runsPerK),
		"1 unique outcome signature per K across all 5 runs, and integrity=ok on every reopen",
		strings.Join(summaryParts, " ; "),
		allRepeatable,
	)

	// KILL-05: invalid KillAt arguments rejected cleanly before spawning strace.
	_, errZero := trace.Run(ctx, trace.Options{
		Command: []string{"true"},
		KillAt:  &trace.KillAt{Syscalls: []string{"fsync"}, When: 0},
	})
	_, errEmpty := trace.Run(ctx, trace.Options{
		Command: []string{"true"},
		KillAt:  &trace.KillAt{Syscalls: nil, When: 1},
	})
	held05 := errZero != nil && errEmpty != nil
	report(
		"KILL-05",
		"trace.Run with invalid KillAt (When=0 and empty Syscalls)",
		"both rejected with non-nil errors before running strace",
		fmt.Sprintf("errZero=%v ; errEmpty=%v", errZero, errEmpty),
		held05,
	)
}
