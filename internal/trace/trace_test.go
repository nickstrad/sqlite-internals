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

// TestParseStraceCLI checks parsing, filtering, path normalization, and formatting on a recorded CLI WAL trace.
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

// TestParseStraceUnfinishedAndKilled verifies resumed calls, errno parsing, and SIGKILL interruption handling.
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

// TestSplitArgsAndUnquote checks top-level comma splitting across annotations and C escape decoding with truncation.
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

// TestParseVFSTrace checks extraction of method names, sizes, offsets, shm lock slots, and relative paths from -vfstrace output.
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

// TestMergeStdoutVFS checks that MergeStdoutVFS pairs each -vfstrace call with the syscalls it encloses.
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

// TestMergeRejectsTruncatedStdout verifies that a truncated fd-1 write causes MergeStdoutVFS to return an error.
func TestMergeRejectsTruncatedStdout(t *testing.T) {
	log, _ := ParseStraceText(`1 write(1<pipe:[1]>, "trace.xWrite(t.db"..., 40) = 40` + "\n")
	if _, err := MergeStdoutVFS(log, 1, nil); err == nil {
		t.Fatal("expected truncation error")
	}
}

// TestLinuxOnlyMessage verifies the prefix and Docker run command produced by LinuxOnlyMessage.
func TestLinuxOnlyMessage(t *testing.T) {
	message := LinuxOnlyMessage("strace of the commit", "ch01-architecture-and-execution-path/experiments/1.2-one-insert")
	if !strings.HasPrefix(message, "LINUX-ONLY: strace of the commit skipped on "+runtime.GOOS) ||
		!strings.HasSuffix(message, "docker compose run --rm lab go run ./ch01-architecture-and-execution-path/experiments/1.2-one-insert") {
		t.Fatalf("message %q", message)
	}
}

// TestWritevMergesLikeWrite confirms that writev fragments on stdout merge identically to write fragments.
func TestWritevMergesLikeWrite(t *testing.T) {
	text := `7 writev(1<pipe:[9]>, [{iov_base="trace.xSync(t.db-wal,FULL)", iov_len=26}, {iov_base=NULL, iov_len=0}], 2) = 26
7 fdatasync(4</w/t.db-wal>) = 0
7 writev(1<pipe:[9]>, [{iov_base=" -> 0", iov_len=5}, {iov_base="\n", iov_len=1}], 2) = 6
7 writev(1<pipe:[9]>, [{iov_base="", iov_len=0}, {iov_base="ok\n", iov_len=3}], 2) = 3
`
	log, err := ParseStraceText(text)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := MergeStdoutVFS(log, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, event := range merged.Relative("/w").Events {
		lines = append(lines, event.String())
	}
	want := "VFS xSync(t.db-wal,FULL) -> 0\n    SYS fdatasync t.db-wal = 0\nOUT ok"
	if strings.Join(lines, "\n") != want {
		t.Fatalf("got\n%s", strings.Join(lines, "\n"))
	}
}

// TestParseStatAndAccessPaths confirms strace parsing resolves paths for stat, newfstatat, statx, and access syscalls.
func TestParseStatAndAccessPaths(t *testing.T) {
	text := `7 newfstatat(AT_FDCWD</w>, "/w/t.db-wal", 0x7ffd, 0) = -1 ENOENT (No such file or directory)
7 newfstatat(AT_FDCWD</w>, "t.db", {st_mode=S_IFREG|0644, st_size=4096, ...}, 0) = 0
7 newfstatat(3</w/t.db>, "", {st_mode=S_IFREG|0644, st_size=0, ...}, AT_EMPTY_PATH) = 0
7 fstat(3</w/t.db>, {st_mode=S_IFREG|0644, st_size=0, ...}) = 0
7 access("/w/t.db-journal", F_OK) = -1 ENOENT (No such file or directory)
7 faccessat2(AT_FDCWD</w>, "t.db-wal", F_OK, 0) = 0
7 statx(AT_FDCWD</w>, "/w/x", AT_STATX_SYNC_AS_STAT, STATX_ALL, 0x7ffd) = -1 ENOENT (No such file or directory)
`
	log, err := ParseStraceText(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"newfstatat /w/t.db-wal = -1 ENOENT",
		"newfstatat /w/t.db = 0",
		"newfstatat /w/t.db = 0",
		"fstat /w/t.db = 0",
		"access /w/t.db-journal = -1 ENOENT",
		"faccessat2 /w/t.db-wal = 0",
		"statx /w/x = -1 ENOENT",
	}
	if len(log.Calls) != len(want) {
		t.Fatalf("got %d calls", len(log.Calls))
	}
	for i, call := range log.Calls {
		if call.String() != want[i] {
			t.Errorf("call %d: %q want %q", i, call.String(), want[i])
		}
	}
}

// TestCanonicalNames checks syscall normalization across open, unlink, sync, stat, access, and pwrite variants.
func TestCanonicalNames(t *testing.T) {
	cases := map[string]string{"openat": "open", "unlinkat": "unlink", "fdatasync": "sync", "fsync": "sync", "newfstatat": "stat", "fstat": "stat", "faccessat2": "access", "pwrite": "pwrite64", "fcntl": "fcntl"}
	for in, want := range cases {
		if got := CanonicalName(in); got != want {
			t.Errorf("%s: %s want %s", in, got, want)
		}
	}
	s := Syscall{Name: "fdatasync", Path: "t.db", FD: 3}
	if s.Canonical().String() != "sync t.db = 0" || !IsSync(s) || IsSync(Syscall{Name: "fcntl"}) {
		t.Fatal(s.Canonical().String())
	}
}
