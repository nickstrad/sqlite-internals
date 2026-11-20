// Package pin holds the SQLite pin and a helper every probe uses to report what it ran against.
package pin

import (
	"database/sql"
	"fmt"
	"strings"
)

const (
	Version  = "3.53.4"
	SourceID = "2026-07-24 19:02:57 bf7c7f30031888f4e796e429ab3978879485813aaca6f641c7b33e4e09459bcc"
)

// Interesting compile options for the course experiments.
var Opts = []string{"ENABLE_SNAPSHOT", "ENABLE_DBPAGE_VTAB", "ENABLE_DBSTAT_VTAB", "ENABLE_SESSION",
	"ENABLE_PREUPDATE_HOOK", "DEBUG", "THREADSAFE", "DEFAULT_WAL_SYNCHRONOUS", "DEFAULT_SYNCHRONOUS", "OS_OTHER", "ENABLE_API_ARMOR"}

// Report prints version, source id, whether it matches the pin, and the interesting compile options.
func Report(route string, db *sql.DB) error {
	var v, id string
	if err := db.QueryRow("select sqlite_version(), sqlite_source_id()").Scan(&v, &id); err != nil {
		return err
	}
	fmt.Printf("route=%s\nsqlite_version=%s\nsqlite_source_id=%s\nmatches_pin=%v\n", route, v, id, id == SourceID)
	rows, err := db.Query("pragma compile_options")
	if err != nil {
		return err
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var o string
		rows.Scan(&o)
		for _, w := range Opts {
			if o == w || strings.HasPrefix(o, w+"=") {
				got = append(got, o)
			}
		}
	}
	fmt.Printf("compile_options(selected)=%s\n", strings.Join(got, " "))
	return rows.Err()
}
