package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickstrad/walfleet/internal/equal"
	_ "modernc.org/sqlite"
)

type doneAfter struct {
	context.Context
	calls  atomic.Int64
	limit  int64
	once   sync.Once
	closed chan struct{}
}

func newDoneAfter(limit int) *doneAfter {
	return &doneAfter{Context: context.Background(), limit: int64(limit), closed: make(chan struct{})}
}

func (c *doneAfter) Done() <-chan struct{} {
	if c.calls.Add(1) >= c.limit {
		c.once.Do(func() { close(c.closed) })
	}
	return c.closed
}

func (c *doneAfter) Err() error {
	select {
	case <-c.closed:
		return context.Canceled
	default:
		return nil
	}
}

func main() {
	iterations := flag.Int("n", 2000, "iterations per mode")
	burners := flag.Int("burn", 0, "CPU-burning goroutines")
	procs := flag.Int("procs", 0, "GOMAXPROCS (0 keeps default)")
	flag.Parse()
	if *procs > 0 {
		runtime.GOMAXPROCS(*procs)
	}
	stop := make(chan struct{})
	for i := 0; i < *burners; i++ {
		go func() {
			x := 0
			for {
				select {
				case <-stop:
					return
				default:
				}
				for j := 0; j < 1e5; j++ {
					x += j
				}
			}
		}()
	}
	dir, _ := os.MkdirTemp("", "fixeq")
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "a.db")
	ctx := context.Background()
	setup, _ := sql.Open("sqlite", path)
	for _, s := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE t(id INTEGER PRIMARY KEY, v)",
		"WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 300) INSERT INTO t SELECT i, zeroblob(50) FROM n"} {
		if _, err := setup.ExecContext(ctx, s); err != nil {
			panic(err)
		}
	}
	setup.Close()
	db, err := equal.OpenExisting(ctx, path)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	beginSuccessReportedAsError := 0
	beginErrors := 0
	for i := 0; i < *iterations; i++ {
		conn, _ := db.Conn(ctx)
		_, err := conn.ExecContext(newDoneAfter(2), "BEGIN")
		if err != nil {
			beginErrors++
			if _, err2 := conn.ExecContext(ctx, "BEGIN"); err2 != nil {
				beginSuccessReportedAsError++
				conn.ExecContext(ctx, "ROLLBACK")
			} else {
				conn.ExecContext(ctx, "ROLLBACK")
			}
		} else {
			conn.ExecContext(ctx, "ROLLBACK")
		}
		conn.Close()
	}
	fmt.Printf("raw BEGIN, Done closes on 2nd call: %d/%d returned an error; %d of those left the transaction open\n", beginErrors, *iterations, beginSuccessReportedAsError)

	leaks := map[int]int{}
	start := time.Now()
	for i := 0; i < *iterations; i++ {
		limit := 1 + i%80
		conn, _ := db.Conn(ctx)
		equal.LogicalHash(newDoneAfter(limit), conn)
		if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
			leaks[limit]++
		}
		conn.ExecContext(ctx, "ROLLBACK")
		conn.Close()
	}
	close(stop)
	total := 0
	for _, n := range leaks {
		total += n
	}
	fmt.Printf("LogicalHash: %d/%d runs left the read transaction open; by Done-limit: %v (INFO: %s)\n", total, *iterations, leaks, time.Since(start).Round(time.Millisecond))
	if total > 0 || beginSuccessReportedAsError > 0 {
		os.Exit(1)
	}
}
