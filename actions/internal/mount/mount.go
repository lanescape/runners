// Package mount tells whether a directory is a mount point, as a process
// sees its mounts in /proc/self/mountinfo.
package mount

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Mounted reports whether dir is a mount point in mountinfo, a file in the
// format of /proc/self/mountinfo: a line per mount, whose fifth field is
// its mount point, with spaces, tabs, newlines and backslashes escaped in
// octal.
func Mounted(mountinfo, dir string) (bool, error) {
	data, err := os.ReadFile(mountinfo)
	if err != nil {
		return false, fmt.Errorf("reading the process's mounts: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}
		if unescape(fields[4]) == dir {
			return true, nil
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("reading the process's mounts: %w", err)
	}
	return false, nil
}

// unescape undoes the kernel's octal escapes, such as \040 for a space.
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
