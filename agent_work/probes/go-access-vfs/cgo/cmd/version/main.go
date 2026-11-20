// Prints the source id of the first-party cgo build and proves the custom compile options work.
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"sqliteinternals/probes/goaccessvfs/cgo/sqlitec"
	"sqliteinternals/probes/goaccessvfs/internal/pin"
)

func main() {
	fmt.Printf("route=first-party-cgo\nsqlite_version=%s\nsqlite_source_id=%s\nmatches_pin=%v\n",
		sqlitec.LibVersion(), sqlitec.SourceID(), sqlitec.SourceID() == pin.SourceID)
	dir, _ := os.MkdirTemp("", "sqlitec-version")
	defer os.RemoveAll(dir)
	db, err := sqlitec.Open(filepath.Join(dir, "t.db"), "")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	must := func(err error) {
		if err != nil {
			log.Fatal(err)
		}
	}
	rows, err := db.Query("select group_concat(compile_options,' ') from pragma_compile_options where compile_options like 'ENABLE_%' or compile_options like 'DEBUG' or compile_options like 'THREADSAFE%' or compile_options like 'DEFAULT_%SYNC%'")
	must(err)
	fmt.Println("compile_options(selected)=", rows[0][0])
	must(db.Exec("pragma journal_mode=wal; create table t(x); insert into t values(1);"))
	frames := 0
	sqlitec.WalHook = func(s string, n int) { frames = n }
	db.InstallWalHook()
	must(db.Exec("insert into t values(2)"))
	fmt.Println("wal_hook frames after insert:", frames)
	snap, err := db.SnapshotGet("main") // must be inside a read txn
	fmt.Println("snapshot_get outside read txn ->", err)
	must(db.Exec("begin"))
	_, err = db.Query("select count(*) from t")
	must(err)
	snap, err = db.SnapshotGet("main")
	must(err)
	must(db.Exec("commit"))
	db2, err := sqlitec.Open(filepath.Join(dir, "t.db"), "")
	must(err)
	must(db2.Exec("insert into t values(3)"))
	must(db.Exec("begin"))
	must(db.SnapshotOpen("main", snap))
	r, _ := db.Query("select count(*) from t")
	fmt.Println("count via old snapshot (expect 2):", r[0][0])
	must(db.Exec("commit"))
	r, _ = db.Query("select count(*) from t")
	fmt.Println("count now (expect 3):", r[0][0])
	snap.Free()
	db2.Close()
	l, c, err := db.CheckpointV2("main", 3)
	fmt.Printf("wal_checkpoint_v2(TRUNCATE): nLog=%d nCkpt=%d err=%v\n", l, c, err)
	v, err := db.FileControlInt("main", sqlitec.FcntlDataVersion, 0)
	fmt.Println("FCNTL_DATA_VERSION:", v, err)
	r, err = db.Query("select pgno, length(data) from sqlite_dbpage where pgno<=2")
	fmt.Println("sqlite_dbpage:", r, err)
	r, err = db.Query("select name, pageno, pagetype from dbstat")
	fmt.Println("dbstat:", r, err)
	fmt.Println("serialize bytes:", len(db.Serialize("main")))
	cw, _, _ := db.DBStatus(sqlitec.DBStatusCacheWrite, false)
	fmt.Println("db_status CACHE_WRITE:", cw)
	r, err = db.Query("select count(*) from pragma_function_list where name like 'sqlite_%'")
	_ = r
}
