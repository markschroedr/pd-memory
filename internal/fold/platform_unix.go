//go:build !windows

package fold

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func lockFile(f *os.File, wait bool) error {
	flags := syscall.LOCK_EX
	if !wait {
		flags |= syscall.LOCK_NB
	}
	e := syscall.Flock(int(f.Fd()), flags)
	if errors.Is(e, syscall.EWOULDBLOCK) {
		return ErrBusy
	}
	return e
}
func unlockFile(f *os.File) { syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
func detach(cmd *exec.Cmd)  { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }
