// Package atomicfile writes files through a same-directory temporary file and
// atomically replaces the destination where the operating system supports it.
package atomicfile

import (
	"io/fs"
	"os"
	"path/filepath"
)

// Write replaces name with contents. The temporary file is created beside the
// destination so the final replacement does not cross filesystem boundaries.
func Write(name string, contents []byte, permission fs.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(name), ".modellink-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)

	if err := temporary.Chmod(permission); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return replace(temporaryName, name)
}
