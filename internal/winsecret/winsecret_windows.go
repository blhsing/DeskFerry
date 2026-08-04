//go:build windows

package winsecret

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ProtectMachine(value string) ([]byte, error) {
	return protect([]byte(value), windows.CRYPTPROTECT_LOCAL_MACHINE)
}

func ProtectCurrentUser(value string) ([]byte, error) {
	return protect([]byte(value), 0)
}

func protect(plain []byte, flags uint32) ([]byte, error) {
	if len(plain) == 0 {
		return nil, nil
	}
	in := windows.DataBlob{Size: uint32(len(plain)), Data: &plain[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, flags|windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return nil, fmt.Errorf("protect secret with DPAPI: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return append([]byte(nil), unsafe.Slice(out.Data, out.Size)...), nil
}

func Unprotect(data []byte) (string, error) {
	if len(data) == 0 {
		return "", nil
	}
	in := windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out); err != nil {
		return "", fmt.Errorf("unprotect secret with DPAPI: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return string(unsafe.Slice(out.Data, out.Size)), nil
}
