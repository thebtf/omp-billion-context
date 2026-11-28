package main

/*
#include <windows.h>
#include <stdint.h>

static DWORD bili_library_path(WCHAR* path, DWORD capacity) {
	HMODULE module;
	if (!GetModuleHandleExW(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
		(LPCWSTR)(uintptr_t)&bili_library_path, &module)) return GetLastError();
	DWORD length = GetModuleFileNameW(module, path, capacity);
	if (length == 0) return GetLastError();
	if (length >= capacity) return ERROR_INSUFFICIENT_BUFFER;
	return ERROR_SUCCESS;
}

static int bili_try_lock(uintptr_t handle) {
	OVERLAPPED position = {0};
	if (LockFileEx((HANDLE)handle, LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &position)) return 0;
	DWORD error = GetLastError();
	return error == ERROR_LOCK_VIOLATION ? -1 : (int)error;
}

static void bili_unlock(uintptr_t handle) {
	OVERLAPPED position = {0};
	UnlockFileEx((HANDLE)handle, 0, 1, 0, &position);
}
*/
import "C"

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

func ownLibraryPath() (string, error) {
	path := make([]uint16, 32768)
	code := syscall.Errno(C.bili_library_path((*C.WCHAR)(unsafe.Pointer(&path[0])), C.DWORD(len(path))))
	if code != 0 {
		return "", code
	}
	return syscall.UTF16ToString(path), nil
}

func tryRuntimeLock(file *os.File) (bool, error) {
	code := int(C.bili_try_lock(C.uintptr_t(file.Fd())))
	switch code {
	case 0:
		return true, nil
	case -1:
		return false, nil
	default:
		return false, syscall.Errno(code)
	}
}

func unlockRuntime(file *os.File) {
	C.bili_unlock(C.uintptr_t(file.Fd()))
}

// Share denial survives a parent-only crash until the child closes its copy. LockFileEx is process-owned.
func trySessionLease(path string) (*os.File, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(abs, `\\?\`) {
		if unc, ok := strings.CutPrefix(abs, `\\`); ok {
			abs = `\\?\UNC\` + unc
		} else {
			abs = `\\?\` + abs
		}
	}
	name, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err == syscall.Errno(C.ERROR_SHARING_VIOLATION) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), path), nil
}

func startWithSessionLease(cmd *exec.Cmd, file *os.File) error {
	handle := syscall.Handle(file.Fd())
	if err := syscall.SetHandleInformation(handle, syscall.HANDLE_FLAG_INHERIT, syscall.HANDLE_FLAG_INHERIT); err != nil {
		return err
	}
	defer syscall.SetHandleInformation(handle, syscall.HANDLE_FLAG_INHERIT, 0)
	cmd.SysProcAttr.AdditionalInheritedHandles = append(cmd.SysProcAttr.AdditionalInheritedHandles, handle)
	return cmd.Start()
}
