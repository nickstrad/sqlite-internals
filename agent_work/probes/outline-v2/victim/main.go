// Probe for the outline revision (Phase 1b): can an unmodified existing driver
// (modernc.org/sqlite v1.60.1, no cgo) serve the trimmed experiment set?
//
//	victim api  <dir>          exercise the driver APIs the outline relies on
//	victim write <db> <sync> N  N one-row WAL commits; prints "ack k" after each
//
// "write" pins its goroutine to one OS thread so that strace's per-thread
// --inject counter sees every SQLite syscall in order.
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"modernc.org/sqlite"
)

const pin = "2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc"

func must(err error) {
	if err != nil {
		fmt.Println("FATAL:", err)
		os.Exit(2)
	}
}

func open(path string, pragmas ...string) *sql.DB {
	dsn := "file:" + path + "?_txlock=immediate"
	for _, p := range pragmas {
		dsn += "&_pragma=" + p
	}
	db, err := sql.Open("sqlite", dsn)
	must(err)
	db.SetMaxOpenConns(1)
	var id string
	must(db.QueryRow("select sqlite_source_id()").Scan(&id))
	if id != pin {
		fmt.Println("FATAL: not the pin:", id)
		os.Exit(3)
	}
	return db
}

func main() {
	switch os.Args[1] {
	case "write":
		runtime.LockOSThread()
		n, _ := strconv.Atoi(os.Args[4])
		db := open(os.Args[2], "journal_mode(WAL)", "synchronous("+os.Args[3]+")", "wal_autocheckpoint(0)")
		_, err := db.Exec("create table if not exists t(id integer primary key, v text)")
		must(err)
		fmt.Println("ready")
		for k := 1; k <= n; k++ {
			_, err := db.Exec("insert into t values(?, ?)", k, fmt.Sprintf("row-%04d", k))
			must(err)
			fmt.Println("ack", k)
		}
		must(db.Close())
	case "api":
		dir := os.Args[2]
		src := filepath.Join(dir, "api.db")
		db := open(src, "journal_mode(WAL)", "busy_timeout(0)")
		fmt.Println("sqlite_source_id matches pin: true")
		_, err := db.Exec("create table t(id integer primary key, v text); insert into t values(1,'a'),(2,'b')")
		must(err)
		var n int
		must(db.QueryRow("select count(*) from sqlite_dbpage").Scan(&n))
		fmt.Println("sqlite_dbpage pages:", n)
		must(db.QueryRow("select count(*) from dbstat").Scan(&n))
		fmt.Println("dbstat rows:", n)
		var busy, log, ckpt int
		must(db.QueryRow("pragma wal_checkpoint(PASSIVE)").Scan(&busy, &log, &ckpt))
		fmt.Println("wal_checkpoint(PASSIVE) busy/log/ckpt:", busy, log, ckpt)
		ctx := context.Background()
		c, err := db.Conn(ctx)
		must(err)
		must(c.Raw(func(dc any) error {
			if s, ok := dc.(interface {
				Status(sqlite.DBStatusOp, bool) (int, int, error)
			}); ok {
				cur, _, err := s.Status(sqlite.DBStatusCacheUsed, false)
				fmt.Println("db_status CACHE_USED bytes:", cur, err)
			} else {
				fmt.Println("db_status: NOT REACHABLE")
			}
			if s, ok := dc.(interface{ Serialize() ([]byte, error) }); ok {
				b, err := s.Serialize()
				fmt.Println("serialize bytes:", len(b), err)
			}
			if s, ok := dc.(interface {
				FileControlDataVersion(string) (uint32, error)
			}); ok {
				v, err := s.FileControlDataVersion("main")
				fmt.Println("FCNTL_DATA_VERSION:", v, err)
			}
			if s, ok := dc.(interface {
				NewBackup(string) (*sqlite.Backup, error)
			}); ok {
				b, err := s.NewBackup(filepath.Join(dir, "backup.db"))
				if err != nil {
					return err
				}
				more, err := b.Step(1)
				fmt.Println("backup step(1): more =", more, "remaining =", b.Remaining(), "of", b.PageCount(), err)
				_, err = b.Step(-1)
				if err != nil {
					return err
				}
				return b.Finish()
			}
			fmt.Println("backup: NOT REACHABLE")
			return nil
		}))
		must(c.Close())
		// Extended result code for a second writer with busy_timeout=0.
		db2 := open(src, "busy_timeout(0)")
		tx, err := db.BeginTx(ctx, nil)
		must(err)
		_, err = db2.Exec("insert into t values(3,'c')")
		var se *sqlite.Error
		if errors.As(err, &se) {
			fmt.Println("second writer error code:", se.Code(), "(5 = SQLITE_BUSY)")
		} else {
			fmt.Println("second writer error:", err)
		}
		must(tx.Rollback())
		must(db2.Close())
		must(db.Close())
		_ = driver.ErrBadConn
	}
}
