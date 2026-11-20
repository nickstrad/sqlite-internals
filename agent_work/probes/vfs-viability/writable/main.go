package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nickstrad/sqlite-internals/agent_work/probes/vfs-viability/shim"
	_ "modernc.org/sqlite"
)

func open(path, journal string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?vfs=logshim&_pragma=journal_mode("+journal+")&_pragma=synchronous(FULL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, db.Ping()
}

func summarize(events []string) string {
	counts := map[string]int{}
	var order []string
	for _, event := range events {
		method := strings.Fields(event)[0]
		if counts[method] == 0 {
			order = append(order, method)
		}
		counts[method]++
	}
	var parts []string
	for _, method := range order {
		parts = append(parts, fmt.Sprintf("%s=%d", method, counts[method]))
	}
	return strings.Join(parts, " ")
}

func scenario(dir, journal string, rows int) error {
	path := filepath.Join(dir, "shim-"+journal+".db")
	shim.Reset()
	db, err := open(path, journal)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)"); err != nil {
		return fmt.Errorf("create: %w", err)
	}
	for i := 1; i <= rows; i++ {
		if _, err := db.Exec("INSERT INTO t VALUES(?, ?)", i, strings.Repeat("v", 300)); err != nil {
			return fmt.Errorf("insert: %w", err)
		}
	}
	var mode string
	db.QueryRow("PRAGMA journal_mode").Scan(&mode)
	if err := db.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	writeEvents := shim.Events()
	shim.Reset()
	db, err = open(path, journal)
	if err != nil {
		return fmt.Errorf("reopen: %w", err)
	}
	var count int
	var check string
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&count); err != nil {
		return fmt.Errorf("count: %w", err)
	}
	db.QueryRow("PRAGMA integrity_check").Scan(&check)
	db.Close()
	fmt.Printf("%s: journal_mode=%s rows_after_reopen=%d integrity=%s\n", journal, mode, count, check)
	fmt.Printf("  write phase VFS calls: %s\n", summarize(writeEvents))
	fmt.Printf("  reopen phase VFS calls: %s\n", summarize(shim.Events()))
	shown := 0
	for _, event := range writeEvents {
		if shown >= 12 {
			break
		}
		shown++
		if strings.HasPrefix(event, "xSync") || strings.HasPrefix(event, "xDelete") || (strings.HasPrefix(event, "xOpen") && !strings.Contains(event, "<temp>")) {
			fmt.Printf("    %s\n", strings.ReplaceAll(event, dir+"/", ""))
		}
	}
	return nil
}

func concurrentWAL(dir string) error {
	path := filepath.Join(dir, "shim-concurrent.db")
	writer, err := open(path, "wal")
	if err != nil {
		return err
	}
	defer writer.Close()
	if _, err := writer.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY)"); err != nil {
		return err
	}
	reader, err := open(path, "wal")
	if err != nil {
		return err
	}
	defer reader.Close()
	tx, err := reader.Begin()
	if err != nil {
		return err
	}
	var before int
	tx.QueryRow("SELECT count(*) FROM t").Scan(&before)
	var wg sync.WaitGroup
	errs := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1; i <= 100; i++ {
			if _, err := writer.Exec("INSERT INTO t VALUES(?)", i); err != nil {
				errs <- err
				return
			}
		}
	}()
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}
	var during int
	tx.QueryRow("SELECT count(*) FROM t").Scan(&during)
	tx.Rollback()
	var after int
	reader.QueryRow("SELECT count(*) FROM t").Scan(&after)
	var busy, logFrames, checkpointed int
	writer.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed)
	fmt.Printf("wal two connections through the shim: reader snapshot before=%d during=%d after_commit_visible=%d checkpoint busy=%d log=%d ckpt=%d\n", before, during, after, busy, logFrames, checkpointed)
	return nil
}

func faults(dir string) error {
	for _, journal := range []string{"delete", "wal"} {
		for _, method := range []string{"xWrite", "xSync"} {
			for nth := 1; nth <= 3; nth++ {
				path := filepath.Join(dir, fmt.Sprintf("fault-%s-%s-%d.db", journal, method, nth))
				db, err := open(path, journal)
				if err != nil {
					return err
				}
				if _, err := db.Exec("CREATE TABLE t(id INTEGER PRIMARY KEY); INSERT INTO t VALUES(1)"); err != nil {
					return err
				}
				shim.FailNth(method, nth, 10)
				_, insertErr := db.Exec("INSERT INTO t VALUES(2)")
				fired := shim.ClearFault()
				db.Close()
				db, err = open(path, journal)
				if err != nil {
					return err
				}
				var count int
				var check string
				db.QueryRow("SELECT count(*) FROM t").Scan(&count)
				db.QueryRow("PRAGMA integrity_check").Scan(&check)
				db.Close()
				result := "ok"
				if insertErr != nil {
					result = insertErr.Error()
				}
				fmt.Printf("fault %s %s #%d fired=%v insert=%q rows_after_reopen=%d integrity=%s\n", journal, method, nth, fired, result, count, check)
			}
		}
	}
	return nil
}

func main() {
	version := int32(3)
	if len(os.Args) > 1 && os.Args[1] == "v1" {
		version = 1
	}
	if err := shim.Register("logshim", version, false); err != nil {
		fmt.Println("register:", err)
		os.Exit(1)
	}
	fmt.Printf("registered logshim with io_methods version %d\n", version)
	dir, err := os.MkdirTemp("", "vfs-viability-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	failed := false
	for _, journal := range []string{"delete", "wal"} {
		if err := scenario(dir, journal, 50); err != nil {
			fmt.Printf("%s: FAILED %v\n", journal, err)
			failed = true
		}
	}
	if err := faults(dir); err != nil {
		fmt.Println("faults: FAILED", err)
		failed = true
	}
	if version >= 2 {
		if err := concurrentWAL(dir); err != nil {
			fmt.Println("wal concurrent: FAILED", err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}
