//go:build !linux

package main

import "errors"

const rlimitSupported = false

func setNoFileLimit(uint64) error {
	return errors.New("setNoFileLimit is Linux-only in this test")
}
