//go:build windows

package wincred

import (
	"errors"
	"fmt"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	TypeGeneric         = 1
	TypeDomainPassword  = 2
	persistLocalMachine = 2
)

type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	advapi32        = windows.NewLazySystemDLL("advapi32.dll")
	procCredReadW   = advapi32.NewProc("CredReadW")
	procCredWriteW  = advapi32.NewProc("CredWriteW")
	procCredDeleteW = advapi32.NewProc("CredDeleteW")
	procCredFree    = advapi32.NewProc("CredFree")
)

func Write(target string, credentialType uint32, user, password string) error {
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	userPtr, err := windows.UTF16PtrFromString(user)
	if err != nil {
		return err
	}
	passwordUTF16, err := windows.UTF16FromString(password)
	if err != nil {
		return err
	}
	passwordUTF16 = passwordUTF16[:len(passwordUTF16)-1]
	var blob *byte
	if len(passwordUTF16) > 0 {
		blob = (*byte)(unsafe.Pointer(&passwordUTF16[0]))
	}
	value := credential{
		Type:               credentialType,
		TargetName:         targetPtr,
		CredentialBlobSize: uint32(len(passwordUTF16) * 2),
		CredentialBlob:     blob,
		Persist:            persistLocalMachine,
		UserName:           userPtr,
	}
	result, _, callErr := procCredWriteW.Call(uintptr(unsafe.Pointer(&value)), 0)
	if result == 0 {
		return fmt.Errorf("write Windows credential %q: %w", target, callErr)
	}
	return nil
}

func Read(target string, credentialType uint32) (user, password string, err error) {
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return "", "", err
	}
	var value *credential
	result, _, callErr := procCredReadW.Call(
		uintptr(unsafe.Pointer(targetPtr)),
		uintptr(credentialType),
		0,
		uintptr(unsafe.Pointer(&value)),
	)
	if result == 0 {
		return "", "", fmt.Errorf("read Windows credential %q: %w", target, callErr)
	}
	defer procCredFree.Call(uintptr(unsafe.Pointer(value)))
	user = windows.UTF16PtrToString(value.UserName)
	if value.CredentialBlob != nil && value.CredentialBlobSize >= 2 {
		units := unsafe.Slice((*uint16)(unsafe.Pointer(value.CredentialBlob)), int(value.CredentialBlobSize/2))
		password = string(utf16.Decode(append([]uint16(nil), units...)))
	}
	return user, password, nil
}

func Delete(target string, credentialType uint32) error {
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	result, _, callErr := procCredDeleteW.Call(uintptr(unsafe.Pointer(targetPtr)), uintptr(credentialType), 0)
	if result == 0 && !errors.Is(callErr, windows.ERROR_NOT_FOUND) {
		return fmt.Errorf("delete Windows credential %q: %w", target, callErr)
	}
	return nil
}
