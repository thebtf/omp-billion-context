//go:build linux || darwin

package main

/*
#cgo linux LDFLAGS: -ldl
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <sys/file.h>

static const char* bili_library_path(void) {
	Dl_info info;
	if (!dladdr((void*)&bili_library_path, &info)) return NULL;
	return info.dli_fname;
}

static int bili_try_lock(int fd) {
	if (flock(fd, LOCK_EX | LOCK_NB) == 0) return 0;
	return errno;
}

static void bili_unlock(int fd) {
	flock(fd, LOCK_UN);
}
*/
import "C"

import (
	"os"
	"os/exec"
	"syscall"
)

func ownLibraryPath() (string, error) {
	path := C.bili_library_path()
	if path == nil {
		return "", errRuntimeStorage
	}
	return C.GoString(path), nil
}

func tryRuntimeLock(file *os.File) (bool, error) {
	code := syscall.Errno(C.bili_try_lock(C.int(file.Fd())))
	switch code {
	case 0:
		return true, nil
	case syscall.EWOULDBLOCK, syscall.EINTR:
		return false, nil
	default:
		return false, code
	}
}

func unlockRuntime(file *os.File) {
	C.bili_unlock(C.int(file.Fd()))
}

func trySessionLease(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if held, err := tryRuntimeLock(file); err != nil || !held {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// flock belongs to the open file description shared with the child. Never issue LOCK_UN on this lease.
func startWithSessionLease(cmd *exec.Cmd, file *os.File) error {
	cmd.ExtraFiles = append(cmd.ExtraFiles, file)
	return cmd.Start()
}
