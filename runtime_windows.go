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
