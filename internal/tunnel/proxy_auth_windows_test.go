//go:build windows

package tunnel

import (
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestProxyAuthSessionIDsPreferConsoleThenActiveThenDisconnected(t *testing.T) {
	sessions := []windows.WTS_SESSION_INFO{
		{SessionID: 4, State: windows.WTSDisconnected},
		{SessionID: 7, State: windows.WTSActive},
		{SessionID: 2, State: windows.WTSActive},
		{SessionID: 9, State: windows.WTSDisconnected},
	}
	want := []uint32{7, 2, 4, 9}
	if got := proxyAuthSessionIDs(7, sessions); !reflect.DeepEqual(got, want) {
		t.Fatalf("proxyAuthSessionIDs() = %v, want %v", got, want)
	}
}
