package main

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nickstrad/walfleet/internal/capture"
	"github.com/nickstrad/walfleet/internal/equal"
	"github.com/nickstrad/walfleet/internal/recipe"
	"github.com/nickstrad/walfleet/internal/trace"

	_ "modernc.org/sqlite"
)

const childMode = "hold-open-write-tx"

func fatal(what string, err error) {
	fmt.Printf("FATAL %s: %v\n", what, err)
	os.Exit(1)
}

func child(path, finish string) {
	ctx := context.Background()
	db, err := recipe.Default().Open(ctx, path)
	if err != nil {
		fatal("child open", err)
	}
	var b, l, c int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&b, &l, &c); err != nil {
		fatal("checkpoint", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		fatal("begin", err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance + 99 WHERE id = 1"); err != nil {
		fatal("update", err)
	}
	fmt.Println("READY")
	buf := make([]byte, 16)
	_, _ = os.Stdin.Read(buf)
	if finish == "commit" {
		err = tx.Commit()
	} else {
		err = tx.Rollback()
	}
	if err != nil {
		fatal(finish, err)
	}
	if err := db.Close(); err != nil {
		fatal("close", err)
	}
}

func balance(path string) string {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		fatal("open", err)
	}
	defer db.Close()
	var v, ic string
	if err := db.QueryRow("SELECT balance FROM accounts WHERE id = 1").Scan(&v); err != nil {
		fatal("balance", err)
	}
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&ic); err != nil {
		fatal("integrity", err)
	}
	return v + " (integrity_check " + ic + ")"
}

func run(dir, finish string) {
	path := filepath.Join(dir, finish+".db")
	if err := recipe.Default().Build(context.Background(), path, 4); err != nil {
		fatal("build", err)
	}
	before := balance(path)
	args, err := trace.SelfCommand(childMode, path, finish)
	if err != nil {
		fatal("self", err)
	}
	cmd := exec.Command(args[0], args[1:]...)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fatal("start", err)
	}
	sc := bufio.NewScanner(stdout)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "READY" {
		fatal("ready", fmt.Errorf("%q", sc.Text()))
	}
	wal, _ := os.Stat(path + "-wal")
	shm, _ := os.Stat(path + "-shm")
	capDir := filepath.Join(dir, finish+"-capture")
	_, capErr := capture.Pages(path, capDir)
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		fatal("child", err)
	}
	fmt.Printf("%s: while the child held an uncommitted write tx: -wal %d bytes, -shm %d bytes; capture.Pages err=%v\n", finish, wal.Size(), shm.Size(), capErr)
	rebuilt := filepath.Join(dir, finish+"-rebuilt.db")
	if _, err := capture.Rebuild(capDir, rebuilt); err != nil {
		fatal("rebuild", err)
	}
	r, err := equal.CompareFiles(path, rebuilt, 4096)
	if err != nil {
		fatal("compare", err)
	}
	fmt.Printf("  balance before the child: %s\n  balance in the rebuilt capture: %s\n  balance in the file after the child %s and closed: %s\n  capture vs file after the child finished: %s\n", before, balance(rebuilt), finish, balance(path), r)
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == childMode {
		child(os.Args[2], os.Args[3])
		return
	}
	dir := filepath.Join("_work", "repair-cap04")
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fatal("mkdir", err)
	}
	run(dir, "rollback")
	run(dir, "commit")
}
