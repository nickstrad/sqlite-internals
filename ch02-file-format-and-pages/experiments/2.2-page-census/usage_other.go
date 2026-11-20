//go:build !unix

package main

func diskUsage(path string) string { return errNoUsage.Error() }
