package trace

import (
	"strings"
	"testing"
)

func parseLines(t *testing.T, lines ...string) *StraceLog {
	t.Helper()
	log, err := ParseStraceText(strings.Join(lines, "\n") + "\n")
	if err != nil {
		t.Fatal(err)
	}
	return log
}

func TestUnfinishedSplitAfterComma(t *testing.T) {
	log := parseLines(t,
		`100 openat(AT_FDCWD</w>, "t.db", <unfinished ...>`,
		`101 close(9) = 0`,
		`100 <... openat resumed>O_RDWR|O_CREAT, 0644) = 3`,
	)
	open := log.Calls[0]
	if open.Path != "/w/t.db" || open.Flags != "O_RDWR|O_CREAT" || open.FD != 3 {
		t.Fatalf("openat %+v", open)
	}
	log = parseLines(t,
		`100 read(3</db/a.db>, <unfinished ...>`,
		`100 <... read resumed>"hello", 10) = 5`,
	)
	read := log.Calls[0]
	if read.Path != "/db/a.db" || read.Data != "hello" || read.Length != 10 || read.Result != 5 {
		t.Fatalf("read %+v", read)
	}
	log = parseLines(t,
		`100 pwrite64(3</db/a.db>, "abc", 3, 4096 <unfinished ...>`,
		`100 <... pwrite64 resumed>) = 3`,
	)
	write := log.Calls[0]
	if write.Length != 3 || write.Offset != 4096 || write.Result != 3 {
		t.Fatalf("pwrite64 %+v", write)
	}
}

func TestRestartedSyscallIsNotASuccess(t *testing.T) {
	log := parseLines(t,
		`100 pwrite64(3</db/a.db>, "abc", 4096, 0) = -1 EINTR (Interrupted system call)`,
		`100 fsync(3</db/a.db> <unfinished ...>`,
		`100 --- SIGURG {si_signo=SIGURG, si_code=SI_TKILL, si_pid=100, si_uid=0} ---`,
		`100 <... fsync resumed>) = ? ERESTARTSYS (To be restarted if SA_RESTART is set)`,
		`100 fsync(3</db/a.db>) = 0`,
	)
	restarted := log.Calls[1]
	if restarted.Succeeded() || restarted.Unfinished || restarted.Errno != "ERESTARTSYS" {
		t.Fatalf("restarted fsync %+v", restarted)
	}
	if got := restarted.String(); got != "fsync /db/a.db = -1 ERESTARTSYS" {
		t.Fatalf("string %q", got)
	}
	if n := len(log.Filter(func(c Syscall) bool { return c.Name == "fsync" && c.Succeeded() })); n != 1 {
		t.Fatalf("succeeded fsyncs %d", n)
	}
}

func TestFDReuseAcrossThreadsWithoutAnnotations(t *testing.T) {
	log := parseLines(t,
		`100 openat(AT_FDCWD, "/db/a.db", O_RDWR|O_CREAT, 0644) = 5`,
		`101 close(5) = 0`,
		`101 openat(AT_FDCWD, "/tmp/other.log", O_RDWR|O_CREAT, 0644) = 5`,
		`100 pwrite64(5, "logdata", 7, 0) = 7`,
		`102 close(5) = 0`,
		`100 pwrite64(5, "x", 1, 0) = -1 EBADF (Bad file descriptor)`,
	)
	if got := log.Calls[3].Path; got != "/tmp/other.log" {
		t.Fatalf("write after cross-thread reuse attributed to %q", got)
	}
	if got := log.Calls[5].Path; got != "" {
		t.Fatalf("write after close attributed to %q", got)
	}
}

func TestDeletedPathAnnotation(t *testing.T) {
	log := parseLines(t,
		`100 pwrite64(3</work/db/gone.db>(deleted), "x", 1, 0) = 1`,
		`100 fsync(3</work/db/gone.db>(deleted)) = 0`,
	)
	matched := log.Filter(UnderDir("/work/db"))
	if len(matched) != 2 || matched[0].Path != "/work/db/gone.db" || matched[0].FD != 3 || matched[1].Name != "fsync" {
		t.Fatalf("matched %+v", matched)
	}
}

func TestWritevLength(t *testing.T) {
	log := parseLines(t,
		`7 writev(1<pipe:[9]>, [{iov_base="trace.xSync(", iov_len=12}, {iov_base="iov_len=99)", iov_len=11}], 2) = 23`,
	)
	call := log.Calls[0]
	if call.Length != 23 || call.Data != "trace.xSync(iov_len=99)" {
		t.Fatalf("writev %+v", call)
	}
	if got := call.String(); got != "writev pipe:[9] len=23 = 23" {
		t.Fatalf("string %q", got)
	}
}

func TestParseVFSLineResultWithParentheses(t *testing.T) {
	call, ok := ParseVFSLine("trace.xRead(t.db,n=100,ofst=0) -> 9999 (0x270f)")
	if !ok || call.Method != "xRead" || call.Amount != 100 || call.Offset != 0 || call.Result != "9999 (0x270f)" || len(call.Args) != 3 {
		t.Fatalf("parsed %+v ok=%v", call, ok)
	}
	call, ok = ParseVFSLine(`trace.xFullPathname(t.db) -> SQLITE_OK, out="/a)b -> (c)"`)
	if !ok || call.File != "t.db" || call.Result != "SQLITE_OK" || call.Detail != `out="/a)b -> (c)"` {
		t.Fatalf("parsed %+v ok=%v", call, ok)
	}
	call, ok = ParseVFSLine("trace.xShmBarrier(t.db)")
	if !ok || call.Result != "" || call.File != "t.db" {
		t.Fatalf("parsed %+v ok=%v", call, ok)
	}
	if _, ok := ParseVFSLine("trace.xSync(t.db-wal,FULL"); ok {
		t.Fatal("unterminated call accepted")
	}
}

func TestMergeSplitPrefixAndFailedWrites(t *testing.T) {
	log := parseLines(t,
		`7 write(1<pipe:[9]>, "trace.", 6) = 6`,
		`7 write(1<pipe:[9]>, "xSync(t.db-wal,FULL)", 20) = 20`,
		`7 fsync(4</w/t.db-wal>) = 0`,
		`7 write(1<pipe:[9]>, " -> 0\n", 6) = 6`,
		`7 write(1<pipe:[9]>, "trace.xClose(t.db) -> SQLITE_OK\n", 32) = -1 EPIPE (Broken pipe)`,
		`7 write(1<pipe:[9]>, "tr", 2) = 2`,
		`7 write(1<pipe:[9]>, "ace.enabled_for(\"unix\")\n", 24) = 24`,
		`7 write(1<pipe:[9]>, "42\nleft", 7) = 3`,
	)
	merged, err := MergeStdoutVFS(log, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, event := range merged.Events {
		lines = append(lines, event.String())
	}
	want := []string{
		"VFS xSync(t.db-wal,FULL) -> 0",
		"    SYS fsync /w/t.db-wal = 0",
		"OUT 42",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if within := merged.SyscallsWithin(0); len(within) != 1 || within[0].Name != "fsync" {
		t.Fatalf("within %+v", within)
	}
}
