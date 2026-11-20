// Runs itself as a child under strace and merges two views into one ordered stream:
//   VFS  lines: the tree's vfstrace shim (each line written to fd 2 by the child, so strace
//               records it in order with everything else)
//   SYS  lines: the real syscalls os_unix.c issued (fd numbers mapped back to file names).
// Phases: WAL insert-commit-checkpoint on w.db, then one commit on d.db in DELETE journal mode
// (which shows the directory fsync that no VFS method exposes).
// Usage: go run ./strace            (needs /usr/bin/strace)
package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"sqliteinternals/probes/goaccessvfs/cgo/sqlitec"
)

func child(dir string) {
	sqlitec.TraceLine = func(s string) { os.Stderr.WriteString("VFS " + s + "\n") }
	must := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	must(sqlitec.RegisterVfsTrace("trace", false))
	mark := func(s string) { os.Stderr.WriteString("PHASE " + s + "\n") }
	db, err := sqlitec.Open(filepath.Join(dir, "w.db"), "trace")
	must(err)
	mark("wal-setup")
	must(db.Exec("pragma journal_mode=wal; create table t(x);"))
	mark("wal-insert-commit")
	must(db.Exec("insert into t values(randomblob(100))"))
	mark("wal-checkpoint")
	_, _, err = db.CheckpointV2("main", 0)
	must(err)
	mark("wal-close")
	must(db.Close())
	mark("delete-mode-setup")
	d, err := sqlitec.Open(filepath.Join(dir, "d.db"), "trace")
	must(err)
	must(d.Exec("pragma journal_mode=delete; create table t(x);"))
	mark("delete-mode-commit")
	must(d.Exec("insert into t values(1)"))
	mark("end")
	must(d.Close())
}

var (
	reCall = regexp.MustCompile(`^(\d+)\s+(\w+)\((.*)\)\s+=\s+(-?\d+|0x[0-9a-f]+)(.*)$`)
	reOpen = regexp.MustCompile(`"([^"]+)"`)
)

func main() {
	if len(os.Args) == 3 && os.Args[1] == "child" {
		child(os.Args[2])
		return
	}
	dir, _ := os.MkdirTemp("", "stracevfs")
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "strace.out")
	self, _ := os.Executable()
	cmd := exec.Command("strace", "-f", "-s", "300", "-o", out,
		"-e", "trace=openat,close,pread64,pwrite64,fsync,fdatasync,fcntl,unlink,unlinkat,ftruncate,write,mmap,munmap",
		self, "child", dir)
	var errBuf strings.Builder
	cmd.Stderr = &errBuf // the child's VFS lines also land here; strace already captured them
	if err := cmd.Run(); err != nil {
		log.Fatalf("%v\n%s", err, errBuf.String())
	}
	f, _ := os.Open(out)
	defer f.Close()
	fds := map[string]string{} // fd -> short name
	phase := "open"
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		m := reCall.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		name, args, ret := m[2], m[3], m[4]
		short := func(s string) string { return strings.ReplaceAll(s, dir+"/", "") }
		a := strings.SplitN(args, ", ", 2)
		fdName := func() string {
			if n, ok := fds[a[0]]; ok {
				return n
			}
			return "fd" + a[0]
		}
		switch name {
		case "write":
			if a[0] == "2" && len(a) > 1 {
				s, _ := strconv.Unquote(strings.SplitN(a[1], `", `, 2)[0] + `"`)
				s = strings.TrimSpace(short(s))
				if strings.HasPrefix(s, "PHASE ") {
					phase = strings.TrimPrefix(s, "PHASE ")
					fmt.Println("==== " + phase)
				} else {
					fmt.Println("  " + s)
				}
			}
		case "openat":
			p := reOpen.FindStringSubmatch(args)
			if p == nil || !strings.HasPrefix(p[1], dir) && p[1] != "." {
				continue
			}
			fds[ret] = short(p[1])
			if p[1] == dir {
				fds[ret] = "<dir>"
			}
			fmt.Printf("      SYS openat(%s) = fd %s\n", fds[ret], ret)
		case "close":
			if n, ok := fds[a[0]]; ok {
				fmt.Printf("      SYS close(%s)\n", n)
				delete(fds, a[0])
			}
		case "mmap":
			parts := strings.Split(args, ", ")
			if len(parts) >= 5 {
				if n, ok := fds[parts[4]]; ok {
					fmt.Printf("      SYS mmap(%s, len=%s, off=%s)\n", n, parts[1], parts[5])
				}
			}
		case "unlink", "unlinkat":
			if strings.Contains(args, dir) {
				fmt.Printf("      SYS %s(%s) = %s\n", name, short(args), ret)
			}
		default:
			if _, ok := fds[a[0]]; ok {
				rest := ""
				if len(a) > 1 {
					rest = a[1]
					if name == "pwrite64" || name == "pread64" {
						p := strings.Split(rest, ", ")
						rest = "n=" + p[len(p)-2] + " off=" + p[len(p)-1]
					}
				}
				fmt.Printf("      SYS %s(%s %s) = %s%s\n", name, fdName(), rest, ret, m[5])
			}
		}
	}
}
