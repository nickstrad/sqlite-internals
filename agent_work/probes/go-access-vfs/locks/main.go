// Cross-process locking interop between the pinned sqlite3 CLI (os_unix.c, POSIX fcntl locks)
// and each Go route, in WAL and DELETE journal modes, both directions. Also, re-executes itself
// under `strace -e trace=fcntl` to show which lock call each route makes.
// Usage: go run ./locks
package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3" // registers "sqlite3"
	ncr "github.com/ncruces/go-sqlite3"
	_ "modernc.org/sqlite" // registers "sqlite"

	"sqliteinternals/probes/goaccessvfs/internal/pin"
)

// route abstracts "run one statement on a fresh connection".
type route struct {
	name string
	open func(path string) (exec func(string) error, close func())
}

func sqlRoute(name, drv string) route {
	return route{name, func(p string) (func(string) error, func()) {
		db, err := sql.Open(drv, p)
		if err != nil {
			log.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		c, err := db.Conn(context.Background())
		if err != nil {
			log.Fatal(err)
		}
		return func(q string) error { _, err := c.ExecContext(context.Background(), q); return err },
			func() { c.Close(); db.Close() }
	}}
}

var routes = []route{
	sqlRoute("mattn", "sqlite3"),
	sqlRoute("modernc", "sqlite"),
	{"ncruces", func(p string) (func(string) error, func()) {
		c, err := ncr.Open(p)
		if err != nil {
			log.Fatal(err)
		}
		return c.Exec, func() { c.Close() }
	}},
}

// hold starts the pinned CLI on path, runs sql (which should leave a transaction open), and
// waits until the CLI has run it (signalled with `.shell touch`; the CLI block-buffers stdout
// on a pipe, so we cannot wait on its output). release commits and quits.
func hold(path, sql string) (release func()) {
	ready := path + ".ready"
	os.Remove(ready)
	cmd := exec.Command("sqlite3", path)
	in, _ := cmd.StdinPipe()
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	io.WriteString(in, ".timeout 0\n"+sql+"\n.shell touch "+ready+"\n")
	for i := 0; ; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if i > 5000 {
			log.Fatal("CLI never became ready")
		}
		time.Sleep(time.Millisecond)
	}
	return func() { io.WriteString(in, "commit;\n.quit\n"); in.Close(); cmd.Wait() }
}

// oneshot runs sql through the pinned CLI with no busy timeout and returns its output.
func oneshot(path, sql string) string {
	b, err := exec.Command("sqlite3", "-cmd", ".timeout 0", path, sql).CombinedOutput()
	s := strings.TrimSpace(string(b))
	if err != nil {
		return fmt.Sprintf("%s (%v)", s, err)
	}
	return "OK " + s
}

func short(err error) string {
	if err == nil {
		return "OK"
	}
	return err.Error()
}

func interop(dir string) {
	for _, mode := range []string{"wal", "delete"} {
		for _, r := range routes {
			p := filepath.Join(dir, r.name+"-"+mode+".db")
			oneshot(p, "pragma journal_mode="+mode+"; create table t(x);")
			// 1. CLI holds a write transaction; the Go route tries to read and to write.
			release := hold(p, "begin immediate; insert into t values('cli');")
			ex, cl := r.open(p)
			ex("pragma busy_timeout=0")
			rerr := ex("select count(*) from t")
			werr := ex("insert into t values('go')")
			cl()
			release()
			fmt.Printf("%-7s %-6s CLI holds write txn -> Go read: %s | Go write: %s\n", r.name, mode, short(rerr), short(werr))
			// 2. Go route holds a write transaction; the CLI tries to read and to write.
			ex, cl = r.open(p)
			ex("pragma busy_timeout=0")
			if err := ex("begin immediate; insert into t values('go2')"); err != nil {
				log.Fatal(err)
			}
			cr := oneshot(p, "select count(*) from t")
			cw := oneshot(p, "insert into t values('cli2')")
			ex("commit")
			cl()
			fmt.Printf("%-7s %-6s Go holds write txn  -> CLI read: %s | CLI write: %s\n", r.name, mode, cr, cw)
			fmt.Printf("%-7s %-6s final rows (CLI): %s\n", r.name, mode, oneshot(p, "select group_concat(x) from t; pragma integrity_check;"))
		}
	}
}

func oneInsert(name, path string) {
	for _, r := range routes {
		if r.name == name {
			ex, cl := r.open(path)
			for _, q := range []string{"pragma journal_mode=wal", "create table if not exists t(x)", "insert into t values(1)"} {
				if err := ex(q); err != nil {
					log.Fatal(err)
				}
			}
			cl()
		}
	}
}

var reCmd = regexp.MustCompile(`fcntl\(\d+, (F_[A-Z_0-9]+)`)

func lockCalls(dir string) {
	self, _ := os.Executable()
	for _, r := range routes {
		out := filepath.Join(dir, r.name+".strace")
		cmd := exec.Command("strace", "-f", "-o", out, "-e", "trace=fcntl,flock", self, "one", r.name, filepath.Join(dir, r.name+"-strace.db"))
		if b, err := cmd.CombinedOutput(); err != nil {
			log.Fatalf("%v %s", err, b)
		}
		b, _ := os.ReadFile(out)
		counts := map[string]int{}
		for _, m := range reCmd.FindAllStringSubmatch(string(b), -1) {
			counts[m[1]]++
		}
		counts["flock"] = strings.Count(string(b), "flock(")
		var ks []string
		for k, v := range counts {
			if v > 0 {
				ks = append(ks, fmt.Sprintf("%s=%d", k, v))
			}
		}
		sort.Strings(ks)
		fmt.Printf("%-7s lock syscalls for open+WAL+create+insert: %s\n", r.name, strings.Join(ks, " "))
	}
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "one" {
		oneInsert(os.Args[2], os.Args[3])
		return
	}
	dir, _ := os.MkdirTemp("", "locks")
	defer os.RemoveAll(dir)
	fmt.Println("pin:", pin.SourceID)
	interop(dir)
	lockCalls(dir)
}
