package fold

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func lockFile(f *os.File, wait bool) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if !wait {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	e := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(e, windows.ERROR_LOCK_VIOLATION) {
		return ErrBusy
	}
	return e
}
func unlockFile(f *os.File) {
	windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
