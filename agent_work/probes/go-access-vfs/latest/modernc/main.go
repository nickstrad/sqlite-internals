// Prints the SQLite version modernc.org/sqlite runs.
package main

import (
	"database/sql"
	"log"

	_ "modernc.org/sqlite"
	"sqliteinternals/probes/goaccessvfs/latest/internal/pin"
)

func main() {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		log.Fatal(err)
	}
	if err := pin.Report("modernc", db); err != nil {
		log.Fatal(err)
	}
}
