//go:build windows

package winservice

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CommandLine reads an installed service's configured binary path using only
// query access, so the non-elevated control panel can detect and migrate it.
func CommandLine(name string) (string, bool, error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return "", false, err
	}
	defer windows.CloseServiceHandle(scm)
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", false, err
	}
	service, err := windows.OpenService(scm, namePtr, windows.SERVICE_QUERY_CONFIG)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer windows.CloseServiceHandle(service)
	var needed uint32
	_ = windows.QueryServiceConfig(service, nil, 0, &needed)
	if needed == 0 {
		return "", true, windows.ERROR_INSUFFICIENT_BUFFER
	}
	buffer := make([]byte, needed)
	config := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buffer[0]))
	if err := windows.QueryServiceConfig(service, config, needed, &needed); err != nil {
		return "", true, err
	}
	return windows.UTF16PtrToString(config.BinaryPathName), true, nil
}
