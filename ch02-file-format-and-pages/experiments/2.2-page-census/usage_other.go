//go:build !unix

package main

// diskUsage reports that sparse-file block allocation is unavailable on non-Unix systems.
func diskUsage(path string) string { return errNoUsage.Error() }
