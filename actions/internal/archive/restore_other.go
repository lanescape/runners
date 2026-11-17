//go:build !linux && !darwin

package archive

import (
	"archive/tar"
	"io"
	"os"
)

func (x *Restorer) dir(string, bool) (*os.File, error) { return nil, errUnsupported }

func (x *Restorer) file(string, *tar.Header, io.Reader, []byte) error { return errUnsupported }

func (x *Restorer) symlink(string, *tar.Header) error { return errUnsupported }

func (x *Restorer) finishDir(*tar.Header) error { return errUnsupported }
