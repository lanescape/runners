package mount

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMounted(t *testing.T) {
	const base = "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n" +
		"25 22 0:21 / /proc rw,nosuid shared:12 - proc proc rw\n"
	for _, tc := range []struct {
		name, info, dir string
		want            bool
	}{
		{"mounted", base + "40 22 8:16 / /work rw,relatime shared:20 - ext4 /dev/sdb rw\n", "/work", true},
		{"the empty directory under it", base, "/work", false},
		{"a mount below it", base + "41 22 8:16 / /work/cache rw - ext4 /dev/sdb rw\n", "/work", false},
		{"a mount named like it", base + "41 22 8:16 / /workspace rw - ext4 /dev/sdb rw\n", "/work", false},
		{"its root mounted elsewhere", base + "41 22 8:16 /work /mnt rw - ext4 /dev/sdb rw\n", "/work", false},
		{"an escaped mount point", base + `41 22 8:16 / /a\040b rw - ext4 /dev/sdb rw` + "\n", "/a b", true},
		{"no final newline", base + "40 22 8:16 / /work rw - ext4 /dev/sdb rw", "/work", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "mountinfo")
			if err := os.WriteFile(p, []byte(tc.info), 0o644); err != nil {
				t.Fatal(err)
			}
			if got, err := Mounted(p, tc.dir); err != nil || got != tc.want {
				t.Fatalf("Mounted = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
	if _, err := Mounted(filepath.Join(t.TempDir(), "none"), "/work"); err == nil {
		t.Fatal("no error for mounts that can't be read")
	}
}
