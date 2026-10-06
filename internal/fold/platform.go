package fold

import (
	"errors"
	"os"
	"path/filepath"
)

var ErrBusy = errors.New("lock busy")

func LockFile(dir, name string, wait bool) (*os.File, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = lockFile(f, wait); e != nil {
		f.Close()
		return nil, e
	}
	return f, nil
}
func Unlock(f *os.File) { unlockFile(f); f.Close() }
