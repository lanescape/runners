//go:build !linux

package main

import "errors"

// The base image is Linux; elsewhere the program builds, for its tests, but
// mounts nothing.
var errNotLinux = errors.New("mounting needs Linux")

func (realSystem) Mount(string, string) error { return errNotLinux }
func (realSystem) Bind(string, string) error  { return errNotLinux }
func (realSystem) Unmount(string) error       { return errNotLinux }
