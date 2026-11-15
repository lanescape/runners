//go:build !linux

package main

// The base image is Linux; elsewhere the program builds, for its tests, but
// measures no filesystem.
func fsUse(string) (size, used int64, ok bool) { return 0, 0, false }
func deviceOf(string) (uint64, bool)           { return 0, false }
