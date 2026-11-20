//go:build sqlite_debug

package amalg

// Build with -tags sqlite_debug to compile the amalgamation with SQLITE_DEBUG (assert()s on,
// PRAGMA vdbe_trace etc.). cgo merges these flags with the ones in sqlitec.go.

// #cgo CFLAGS: -DSQLITE_DEBUG
import "C"
