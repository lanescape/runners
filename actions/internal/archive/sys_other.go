//go:build !linux && !darwin

package archive

import (
	"errors"
	"io/fs"
	"os"
	"time"
)

// Caches are saved and restored on the runner service's machines, which
// run Linux; elsewhere the programs build, and do nothing of this.

var errUnsupported = errors.New("caches are saved and restored only on Linux and macOS")

func owner(fs.FileInfo) (uid, gid int) { return 0, 0 }

func openDir(*os.File, string) (*os.File, error) { return nil, errUnsupported }

func openFile(*os.File, string) (*os.File, error) { return nil, errUnsupported }

func readlinkIn(*os.File, string) (string, error) { return "", errUnsupported }

func lstatIn(*os.File, string) (fs.FileInfo, error) { return nil, errUnsupported }

func lchtimes(*os.Root, string, time.Time) error { return errUnsupported }
