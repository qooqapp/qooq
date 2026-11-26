//go:build !unix

package main

import "errors"

func deviceOf(string) (uint64, error) { return 0, errors.New("not supported on this platform") }

func sameFilesystem(string, string) (bool, error) {
	return false, errors.New("not supported on this platform")
}

func freeDisk(string) (uint64, uint64, error) {
	return 0, 0, errors.New("free disk check is not supported on this platform")
}
