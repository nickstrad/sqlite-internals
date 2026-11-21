// Command trace-parser runs adversarial attacks against internal/trace's strace
// parser and stdout VFS merger using both real strace 6.8 traces and tricky
// synthetic strace streams.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// childLongPath opens, pwrites, fsyncs, and closes a file at a >600-byte path.
func childLongPath(target string) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := f.WriteAt([]byte("long-path-payload"), 0); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := f.Sync(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

// childDeletedFile opens a file, unlinks it while open, then pwrites, fsyncs, and closes it.
func childDeletedFile(target string) {
	f, err := os.OpenFile(target, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := os.Remove(target); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := f.WriteAt([]byte("after-unlink"), 0); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := f.Sync(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "child-longpath" {
		childLongPath(os.Args[2])
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "child-deleted" {
		childDeletedFile(os.Args[2])
		return
	}

	repoRoot, err := sqlitex.RepoRoot()
	if err != nil {
		fatal("locate repo root", err)
	}
	workDir := filepath.Join(repoRoot, "_work", "adv-ch00", "trace-parser")
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

	// TRC-01: unfinished/resumed split across interleaved threads (strace 6.8 double-space format).
	trc01Text := strings.Join([]string{
		`100 pwrite64(3</db/a.db>, "abc", 4096, 0 <unfinished ...>`,
		`101 pwrite64(4</db/b.db>, "xyz", 512, 4096) = 512`,
		`100 <... pwrite64 resumed>) = 4096`,
		`100 read(3</db/a.db>,  <unfinished ...>`,
		`101 write(4</db/b.db>, "ok", 2) = 2`,
		`100 <... read resumed>"hello", 10) = 5`,
	}, "\n") + "\n"
	log01, err := trace.ParseStraceText(trc01Text)
	if err != nil {
		fatal("TRC-01 parse", err)
	}
	held01 := len(log01.Calls) == 4 &&
		log01.Calls[0].PID == 100 && log01.Calls[0].Path == "/db/a.db" && log01.Calls[0].Offset == 0 && log01.Calls[0].Length == 4096 && log01.Calls[0].Result == 4096 &&
		log01.Calls[1].PID == 101 && log01.Calls[1].Path == "/db/b.db" && log01.Calls[1].Offset == 4096 && log01.Calls[1].Length == 512 &&
		log01.Calls[2].PID == 100 && log01.Calls[2].Path == "/db/a.db" && log01.Calls[2].Data == "hello" && log01.Calls[2].Length == 10 && log01.Calls[2].Result == 5 &&
		log01.Calls[3].PID == 101 && log01.Calls[3].Path == "/db/b.db" && log01.Calls[3].Data == "ok"
	report(
		"TRC-01",
		"interleaved <unfinished ...> / <... resumed> calls across PIDs 100 and 101 (strace 6.8 two-space comma split)",
		"4 calls reconstructed with exact PID, Path, Offset, Length, Data, Result and no cross-thread bleed",
		fmt.Sprintf("calls=%d c0=(%s len=%d off=%d res=%d) c2=(%s data=%q len=%d res=%d)",
			len(log01.Calls), log01.Calls[0].Path, log01.Calls[0].Length, log01.Calls[0].Offset, log01.Calls[0].Result,
			log01.Calls[2].Path, log01.Calls[2].Data, log01.Calls[2].Length, log01.Calls[2].Result),
		held01,
	)

	// TRC-02: unfinished/resumed with single space between comma and <unfinished ...>.
	trc02Text := strings.Join([]string{
		`100 read(3</db/a.db>, <unfinished ...>`,
		`100 <... read resumed>"hello", 10) = 5`,
	}, "\n") + "\n"
	log02, err := trace.ParseStraceText(trc02Text)
	if err != nil {
		fatal("TRC-02 parse", err)
	}
	held02 := len(log02.Calls) == 1 && log02.Calls[0].Data == "hello" && log02.Calls[0].Length == 10 && log02.Calls[0].Result == 5
	report(
		"TRC-02",
		"unfinished/resumed split with single space after comma: read(3</db/a.db>, <unfinished ...>",
		"reconstructed call has Data=\"hello\" and Length=10 (no doubled comma shifting args)",
		fmt.Sprintf("calls=%d path=%q data=%q len=%d res=%d",
			len(log02.Calls), log02.Calls[0].Path, log02.Calls[0].Data, log02.Calls[0].Length, log02.Calls[0].Result),
		held02,
	)

	// TRC-03: interrupted syscalls: EINTR and real strace 6.8 ERESTARTSYS.
	trc03Text := strings.Join([]string{
		`100 pwrite64(3</db/a.db>, "abc", 4096, 0) = -1 EINTR (Interrupted system call)`,
		`100 fsync(3</db/a.db> <unfinished ...>`,
		`100 --- SIGURG {si_signo=SIGURG, si_code=SI_TKILL, si_pid=100, si_uid=0} ---`,
		`100 <... fsync resumed>) = ? ERESTARTSYS (To be restarted if SA_RESTART is set)`,
		`100 fsync(3</db/a.db>) = 0`,
	}, "\n") + "\n"
	log03, err := trace.ParseStraceText(trc03Text)
	if err != nil {
		fatal("TRC-03 parse", err)
	}
	eintrCall := log03.Calls[0]
	erestartCall := log03.Calls[1]
	restartedCall := log03.Calls[2]
	succeededFsyncs := 0
	for _, c := range log03.Calls {
		if c.Name == "fsync" && c.Succeeded() {
			succeededFsyncs++
		}
	}
	held03 := !eintrCall.Succeeded() && eintrCall.Errno == "EINTR" &&
		!erestartCall.Succeeded() &&
		restartedCall.Succeeded() &&
		succeededFsyncs == 1
	report(
		"TRC-03",
		"interrupted syscalls: EINTR on pwrite64 and strace 6.8 '? ERESTARTSYS' on fsync followed by restarted fsync",
		"EINTR and ERESTARTSYS both have Succeeded()==false; only 1 succeeded fsync counted",
		fmt.Sprintf("eintr=(ok=%v errno=%q) erestart=(ok=%v unfinished=%v errno=%q res=%d) succeededFsyncs=%d",
			eintrCall.Succeeded(), eintrCall.Errno, erestartCall.Succeeded(), erestartCall.Unfinished, erestartCall.Errno, erestartCall.Result, succeededFsyncs),
		held03,
	)

	// TRC-04: long path (>600 bytes) via live strace on Linux (or synthetic on !linux).
	deepDir := realWorkDir
	for i := range 6 {
		deepDir = filepath.Join(deepDir, fmt.Sprintf("dir_%s_%d", strings.Repeat("x", 92), i))
	}
	if err := os.MkdirAll(deepDir, 0o755); err != nil {
		fatal("TRC-04 mkdir deep", err)
	}
	longPath := filepath.Join(deepDir, "longname.db")
	if trace.Supported {
		cmd, err := trace.SelfCommand("child-longpath", longPath)
		if err != nil {
			fatal("TRC-04 self command", err)
		}
		res, err := trace.Run(context.Background(), trace.Options{Command: cmd})
		if err != nil {
			fatal("TRC-04 trace.Run", err)
		}
		under := res.Log.Filter(trace.UnderDir(realWorkDir))
		var gotOpen, gotWrite, gotSync, gotClose bool
		for _, c := range under {
			if c.Path != longPath {
				continue
			}
			switch c.Name {
			case "openat", "open":
				gotOpen = true
			case "pwrite64":
				gotWrite = true
			case "fsync", "fdatasync":
				gotSync = true
			case "close":
				gotClose = true
			}
		}
		held04 := len(longPath) > 600 && gotOpen && gotWrite && gotSync && gotClose
		report(
			"TRC-04",
			fmt.Sprintf("live strace on %d-byte nested path (exceeds default -s 512 buffer limit)", len(longPath)),
			"open/openat, pwrite64, fsync, close all attribute exact full path and match UnderDir",
			fmt.Sprintf("pathLen=%d open=%v pwrite64=%v fsync=%v close=%v", len(longPath), gotOpen, gotWrite, gotSync, gotClose),
			held04,
		)
	} else {
		synth := fmt.Sprintf("100 openat(AT_FDCWD, %q, O_RDWR|O_CREAT, 0644) = 3<%s>\n100 pwrite64(3<%s>, \"x\", 1, 0) = 1\n100 fsync(3<%s>) = 0\n", longPath, longPath, longPath, longPath)
		l, parseErr := trace.ParseStraceText(synth)
		if parseErr != nil {
			fatal("TRC-04 synth", parseErr)
		}
		under := l.Filter(trace.UnderDir(realWorkDir))
		held04 := len(longPath) > 600 && len(under) == 3
		report("TRC-04", fmt.Sprintf("synthetic strace on %d-byte path", len(longPath)), "all 3 calls match UnderDir", fmt.Sprintf("matched=%d/3", len(under)), held04)
	}

	// TRC-05: single-PID FD reuse after close without -y annotations.
	trc05Text := strings.Join([]string{
		`100 openat(AT_FDCWD, "/db/a.db", O_RDWR|O_CREAT, 0644) = 5`,
		`100 pwrite64(5, "page1", 4096, 0) = 4096`,
		`100 close(5) = 0`,
		`100 openat(AT_FDCWD, "/tmp/other.log", O_RDWR|O_CREAT, 0644) = 5`,
		`100 pwrite64(5, "logentry", 8, 0) = 8`,
		`100 fsync(5) = 0`,
	}, "\n") + "\n"
	log05, err := trace.ParseStraceText(trc05Text)
	if err != nil {
		fatal("TRC-05 parse", err)
	}
	dbCalls05 := log05.Filter(trace.UnderDir("/db"))
	held05 := log05.Calls[1].Path == "/db/a.db" &&
		log05.Calls[4].Path == "/tmp/other.log" &&
		log05.Calls[5].Path == "/tmp/other.log" &&
		len(dbCalls05) == 3 // openat, pwrite64, close
	report(
		"TRC-05",
		"single-PID fd 5 reuse after close (no -y): /db/a.db closed, fd 5 reopened as /tmp/other.log",
		"writes/fsync after reopen attributed to /tmp/other.log, never /db/a.db",
		fmt.Sprintf("w1=%q w2=%q sync2=%q dbCalls=%d", log05.Calls[1].Path, log05.Calls[4].Path, log05.Calls[5].Path, len(dbCalls05)),
		held05,
	)

	// TRC-06: multi-thread FD close+reuse without -y annotations (TID 100 opens fd 5 = /db/a.db,
	// TID 101 closes fd 5 and reopens fd 5 = /tmp/other.log, TID 100 writes to fd 5).
	trc06Text := strings.Join([]string{
		`100 openat(AT_FDCWD, "/db/a.db", O_RDWR|O_CREAT, 0644) = 5`,
		`101 close(5) = 0`,
		`101 openat(AT_FDCWD, "/tmp/other.log", O_RDWR|O_CREAT, 0644) = 5`,
		`100 pwrite64(5, "logdata", 7, 0) = 7`,
	}, "\n") + "\n"
	log06, err := trace.ParseStraceText(trc06Text)
	if err != nil {
		fatal("TRC-06 parse", err)
	}
	gotPath06 := log06.Calls[3].Path
	held06 := gotPath06 == "/tmp/other.log"
	report(
		"TRC-06",
		"cross-thread fd 5 close+reopen (no -y): TID 100 opens /db/a.db, TID 101 closes & opens /tmp/other.log, TID 100 writes fd 5",
		"TID 100 write attributed to /tmp/other.log (not stale /db/a.db)",
		fmt.Sprintf("tid100WritePath=%q", gotPath06),
		held06,
	)

	// TRC-07: writes to non-database files, pipes, /dev/null, and sibling prefix directory /work/db2/.
	trc07Text := strings.Join([]string{
		`100 write(1<pipe:[99]>, "hello\n", 6) = 6`,
		`100 write(2</dev/null>, "err\n", 4) = 4`,
		`100 pwrite64(6</tmp/other.log>, "noise", 5, 0) = 5`,
		`100 pwrite64(7</work/db2/sneaky.db>, "sibling", 7, 0) = 7`,
		`100 pwrite64(8</work/db/real.db>, "page", 4096, 0) = 4096`,
		`100 openat(AT_FDCWD</work/db>, "../db2/escape.log", O_RDWR|O_CREAT, 0644) = 9`,
	}, "\n") + "\n"
	log07, err := trace.ParseStraceText(trc07Text)
	if err != nil {
		fatal("TRC-07 parse", err)
	}
	under07 := log07.Filter(trace.UnderDir("/work/db"))
	held07 := len(under07) == 1 && under07[0].Path == "/work/db/real.db"
	var paths07 []string
	for _, c := range under07 {
		paths07 = append(paths07, c.Name+":"+c.Path)
	}
	report(
		"TRC-07",
		"UnderDir(/work/db) against pipe, /dev/null, /tmp/other.log, sibling /work/db2/, and ../db2/escape.log",
		"only /work/db/real.db matched (1 call)",
		fmt.Sprintf("matched=%d %v", len(under07), paths07),
		held07,
	)

	// TRC-08: unlinked-while-open file with strace 6.8's `fd</path>(deleted)` suffix.
	if trace.Supported {
		delTarget := filepath.Join(realWorkDir, "unlinked-open.db")
		cmd, err := trace.SelfCommand("child-deleted", delTarget)
		if err != nil {
			fatal("TRC-08 self command", err)
		}
		res, err := trace.Run(context.Background(), trace.Options{Command: cmd})
		if err != nil {
			fatal("TRC-08 trace.Run", err)
		}
		under08 := res.Log.Filter(trace.UnderDir(realWorkDir))
		var sawWrite, sawSync bool
		for _, c := range under08 {
			if c.Name == "pwrite64" {
				sawWrite = true
			}
			if c.Name == "fsync" || c.Name == "fdatasync" {
				sawSync = true
			}
		}
		// Find the raw pwrite64 line containing (deleted) to show exact strace 6.8 syntax.
		var rawSample string
		for _, line := range strings.Split(res.Text, "\n") {
			if strings.Contains(line, "unlinked-open.db") && strings.Contains(line, "pwrite64") {
				rawSample = strings.TrimSpace(line)
				break
			}
		}
		held08 := sawWrite && sawSync
		report(
			"TRC-08",
			"live strace 6.8 on unlinked-while-open file emitting fd</path>(deleted) on pwrite64 and fsync",
			"UnderDir captures pwrite64 and fsync on the unlinked-while-open fd",
			fmt.Sprintf("sawWrite=%v sawSync=%v raw=%q", sawWrite, sawSync, rawSample),
			held08,
		)
	} else {
		synth := "100 pwrite64(3</work/db/gone.db>(deleted), \"x\", 1, 0) = 1\n100 fsync(3</work/db/gone.db>(deleted)) = 0\n"
		l, _ := trace.ParseStraceText(synth)
		under08 := l.Filter(trace.UnderDir("/work/db"))
		report("TRC-08", "synthetic strace 6.8 fd</path>(deleted) suffix", "UnderDir captures both calls", fmt.Sprintf("matched=%d/2", len(under08)), len(under08) == 2)
	}

	// TRC-09: MergeStdoutVFS when stdout splits the "trace.x" prefix across two write calls.
	trc09Text := strings.Join([]string{
		`7 write(1<pipe:[9]>, "trace.", 6) = 6`,
		`7 write(1<pipe:[9]>, "xSync(t.db-wal,FULL)", 20) = 20`,
		`7 fsync(4</w/t.db-wal>) = 0`,
		`7 write(1<pipe:[9]>, " -> 0\n", 6) = 6`,
	}, "\n") + "\n"
	log09, err := trace.ParseStraceText(trc09Text)
	if err != nil {
		fatal("TRC-09 parse", err)
	}
	merged09, err := trace.MergeStdoutVFS(log09, 1, nil)
	if err != nil {
		fatal("TRC-09 merge", err)
	}
	vfsCalls09 := merged09.VFSCalls()
	enclosedSync := false
	for i, ev := range merged09.Events {
		if ev.Kind == trace.EventVFS && ev.VFS.Method == "xSync" {
			for _, sc := range merged09.SyscallsWithin(i) {
				if sc.Name == "fsync" {
					enclosedSync = true
				}
			}
		}
	}
	held09 := len(vfsCalls09) == 1 && vfsCalls09[0].Method == "xSync" && enclosedSync
	report(
		"TRC-09",
		"MergeStdoutVFS when stdout splits 'trace.' and 'xSync(t.db-wal,FULL)' across two write(1) calls",
		"1 VFS xSync event recognized with fsync enclosed inside it",
		fmt.Sprintf("vfsCalls=%d events=%d enclosedSync=%v", len(vfsCalls09), len(merged09.Events), enclosedSync),
		held09,
	)
}
