package trace

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

func readTestdata(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParseStraceCLI(t *testing.T) {
	log, err := ParseStraceText(readTestdata(t, "cli-wal.strace"))
	if err != nil {
		t.Fatal(err)
	}
	files := log.Filter(UnderDir("/w"))
	var lines []string
	for _, call := range RelativeTo("/w", files) {
		lines = append(lines, call.String())
	}
	want := []string{
		"openat t.db O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd",
		"fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1 = 0",
		"fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0",
		"fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1 = 0",
		"openat t.db-wal O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd",
		"pwrite64 t.db-wal off=32 len=24 = 24",
		"fsync t.db-wal = 0",
		"openat t.db-shm O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd",
		"ftruncate t.db-shm len=3 = 0",
		"mmap t.db-shm off=0 len=32768 = ok",
		"unlink t.db-wal = 0",
		"close t.db-wal = 0",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if len(log.Exits) != 1 || log.Exits[0].Code != 0 || log.KilledBy() != "" {
		t.Fatalf("exits %+v", log.Exits)
	}
	pw := log.Filter(Named("pwrite64"))[0]
	if !pw.Truncated || pw.FD != 4 || pw.PID != 4242 {
		t.Fatalf("pwrite64 %+v", pw)
	}
}

func TestParseStraceUnfinishedAndKilled(t *testing.T) {
	log, err := ParseStraceText(readTestdata(t, "go-killed.strace"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, call := range log.Calls {
		names = append(names, call.String())
	}
	want := []string{
		"openat /w/g.db O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd",
		"openat /w/other O_RDONLY = -1 ENOENT",
		"pwrite64 /w/g.db off=0 len=4096 = 4096",
		"futex = 1",
		"fdatasync /w/g.db = 0",
		"pread64 /w/g.db off=0 len=100 = 100",
		"fcntl /w/g.db F_GETLK F_UNLCK start=128 len=1 = 0",
		"fcntl /w/g.db F_SETFD = 0",
		"openat /w/g.db-wal O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd",
		"unlinkat /w/g.db-journal = 0",
		"close /w/g.db-wal = 0",
		"fsync /w/g.db = ? (process killed)",
	}
	if strings.Join(names, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	if log.KilledBy() != "SIGKILL" || len(log.Exits) != 2 {
		t.Fatalf("exits %+v", log.Exits)
	}
	if log.Calls[1].Errno != "ENOENT" || log.Calls[1].Succeeded() {
		t.Fatalf("errno %+v", log.Calls[1])
	}
	if log.Calls[2].Index != 2 || !strings.Contains(log.Calls[2].Raw, "resumed") {
		t.Fatalf("resumed call %+v", log.Calls[2])
	}
}

func TestSplitArgsAndUnquote(t *testing.T) {
	args := SplitArgs(`3</a, b/c.db>, "x, \"y\"", {l_type=F_RDLCK, l_len=1}, [1, 2], 7`)
	want := []string{`3</a, b/c.db>`, `"x, \"y\""`, `{l_type=F_RDLCK, l_len=1}`, `[1, 2]`, `7`}
	if strings.Join(args, "|") != strings.Join(want, "|") {
		t.Fatalf("split %q", args)
	}
	text, truncated := Unquote(`"a\nb\t\"q\"\\\0\377\x41"...`)
	if text != "a\nb\t\"q\"\\\x00\xffA" || !truncated {
		t.Fatalf("unquote %q %v", text, truncated)
	}
}

func TestParseVFSTrace(t *testing.T) {
	parsed, err := ParseVFSTrace(strings.NewReader(readTestdata(t, "cli-wal.vfstrace")))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Calls) != 13 || len(parsed.Output) != 1 || parsed.Output[0] != "wal" || len(parsed.Other) != 1 {
		t.Fatalf("calls %d output %q other %q", len(parsed.Calls), parsed.Output, parsed.Other)
	}
	write := parsed.Methods("xWrite")[0]
	if write.File != "t.db-wal" || write.Amount != 4096 || write.Offset != 56 || write.Result != "SQLITE_OK" {
		t.Fatalf("xWrite %+v", write)
	}
	shm := parsed.Methods("xShmLock")[0]
	if shm.ShmSlot != "READ1" || shm.Offset != 4 || shm.Amount != 1 || shm.Flag != "LOCK|SHARED" {
		t.Fatalf("xShmLock %+v", shm)
	}
	sync := parsed.Methods("xSync")[0]
	if sync.Flag != "FULL" || sync.Result != "0" {
		t.Fatalf("xSync %+v", sync)
	}
	truncate := parsed.Methods("xTruncate")[0]
	if truncate.Amount != 8192 {
		t.Fatalf("xTruncate %+v", truncate)
	}
	pragma := parsed.Methods("xFileControl")[0]
	if pragma.Flag != "PRAGMA" || len(pragma.Args) != 3 {
		t.Fatalf("xFileControl %+v", pragma)
	}
	size := parsed.Methods("xFileSize")[0]
	if size.Detail != "size=0" {
		t.Fatalf("xFileSize %+v", size)
	}
	barrier := parsed.Methods("xShmBarrier")[0]
	if barrier.Result != "" || barrier.File != "t.db" {
		t.Fatalf("xShmBarrier %+v", barrier)
	}
	del := RelativeVFS("/w", parsed.Methods("xDelete"))[0]
	if del.File != "t.db-wal" || del.String() != `xDelete("t.db-wal",0) -> SQLITE_OK` {
		t.Fatalf("xDelete %+v %s", del, del)
	}
	if _, ok := ParseVFSLine("wal"); ok {
		t.Fatal("plain output parsed as a VFS call")
	}
}

func TestMergeStdoutVFS(t *testing.T) {
	log, err := ParseStraceText(readTestdata(t, "cli-wal.strace"))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeStdoutVFS(log, 1, UnderDir("/w"))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, event := range merged.Relative("/w").Events {
		lines = append(lines, event.String())
	}
	want := []string{
		"SYS openat t.db O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd",
		"VFS xOpen(t.db,flags=0x106) -> SQLITE_OK, outFlags=0x106",
		"VFS xLock(t.db,SHARED) -> SQLITE_OK",
		"    SYS fcntl t.db F_SETLK F_RDLCK start=1073741824 len=1 = 0",
		"    SYS fcntl t.db F_SETLK F_RDLCK start=1073741826 len=510 = 0",
		"    SYS fcntl t.db F_SETLK F_UNLCK start=1073741824 len=1 = 0",
		"OUT wal",
		"SYS openat t.db-wal O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd",
		"VFS xWrite(t.db-wal,n=24,ofst=32) -> SQLITE_OK",
		"    SYS pwrite64 t.db-wal off=32 len=24 = 24",
		"VFS xSync(t.db-wal,FULL) -> 0",
		"    SYS fsync t.db-wal = 0",
		"VFS xShmMap(t.db,iRegion=0,szRegion=32768,isWrite=0,*) -> SQLITE_OK",
		"    SYS openat t.db-shm O_RDWR|O_CREAT|O_NOFOLLOW|O_CLOEXEC = fd",
		"    SYS ftruncate t.db-shm len=3 = 0",
		"    SYS mmap t.db-shm off=0 len=32768 = ok",
		`VFS xDelete("t.db-wal",0) -> SQLITE_OK`,
		"    SYS unlink t.db-wal = 0",
		"SYS close t.db-wal = 0",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
	if len(merged.VFSCalls()) != 6 {
		t.Fatalf("vfs calls %d", len(merged.VFSCalls()))
	}
	syncEvent := -1
	for i, event := range merged.Events {
		if event.Kind == EventVFS && event.VFS.Method == "xSync" {
			syncEvent = i
		}
	}
	within := merged.SyscallsWithin(syncEvent)
	if len(within) != 1 || within[0].Name != "fsync" {
		t.Fatalf("within xSync: %+v", within)
	}
}

func TestMergeRejectsTruncatedStdout(t *testing.T) {
	log, _ := ParseStraceText(`1 write(1<pipe:[1]>, "trace.xWrite(t.db"..., 40) = 40` + "\n")
	if _, err := MergeStdoutVFS(log, 1, nil); err == nil {
		t.Fatal("expected truncation error")
	}
}

func TestLinuxOnlyMessage(t *testing.T) {
	message := LinuxOnlyMessage("strace of the commit", "ch01-architecture-and-execution-path/experiments/1.2-one-insert")
	if !strings.HasPrefix(message, "LINUX-ONLY: strace of the commit skipped on "+runtime.GOOS) ||
		!strings.HasSuffix(message, "docker compose run --rm lab go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert") {
		t.Fatalf("message %q", message)
	}
}
