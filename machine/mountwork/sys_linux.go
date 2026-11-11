//go:build linux

package main

import "syscall"

// Mount mounts an ext4 filesystem, with discard and without access times.
func (realSystem) Mount(device, target string) error {
	return syscall.Mount(device, target, "ext4", syscall.MS_NOATIME, "discard")
}

func (realSystem) Bind(source, target string) error {
	return syscall.Mount(source, target, "", syscall.MS_BIND, "")
}

func (realSystem) Unmount(target string) error { return syscall.Unmount(target, 0) }
