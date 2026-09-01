//go:build windows

package tunnel

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/alexbrainman/sspi"
	"github.com/alexbrainman/sspi/ntlm"
	"golang.org/x/sys/windows"
)

type windowsNTLMProxyAuthenticator struct {
	credentials *sspi.Credentials
	context     *ntlm.ClientContext
	initial     []byte
}

func newIntegratedProxyAuthenticator() (integratedProxyAuthenticator, error) {
	credentials, err := acquireProxyCredentials()
	if err != nil {
		return nil, err
	}
	context, initial, err := ntlm.NewClientContext(credentials)
	if err != nil {
		_ = credentials.Release()
		return nil, err
	}
	return &windowsNTLMProxyAuthenticator{
		credentials: credentials,
		context:     context,
		initial:     initial,
	}, nil
}

func acquireProxyCredentials() (*sspi.Credentials, error) {
	if !isLocalSystem() {
		return ntlm.AcquireCurrentUserCredentials()
	}
	credentials, interactiveErr := acquireInteractiveUserCredentials()
	if interactiveErr == nil {
		return credentials, nil
	}
	credentials, systemErr := ntlm.AcquireCurrentUserCredentials()
	if systemErr == nil {
		return credentials, nil
	}
	return nil, errors.Join(
		fmt.Errorf("acquire logged-on Windows user credentials: %w", interactiveErr),
		fmt.Errorf("acquire LocalSystem credentials: %w", systemErr),
	)
}

func isLocalSystem() bool {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	return err == nil && user.User.Sid.String() == "S-1-5-18"
}

func acquireInteractiveUserCredentials() (*sspi.Credentials, error) {
	token, err := interactiveUserToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()

	var impersonationToken windows.Token
	if err := windows.DuplicateTokenEx(
		token,
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_IMPERSONATE,
		nil,
		windows.SecurityImpersonation,
		windows.TokenImpersonation,
		&impersonationToken,
	); err != nil {
		return nil, fmt.Errorf("duplicate logged-on Windows user token: %w", err)
	}
	defer impersonationToken.Close()

	// Windows impersonation is thread-affine. Keep AcquireCredentialsHandle on
	// this OS thread, then immediately return the thread to LocalSystem. The
	// resulting SSPI credential handle remains bound to the logged-on user.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.SetThreadToken(nil, impersonationToken); err != nil {
		return nil, fmt.Errorf("impersonate logged-on Windows user: %w", err)
	}
	credentials, acquireErr := ntlm.AcquireCurrentUserCredentials()
	revertErr := windows.RevertToSelf()
	if acquireErr != nil {
		return nil, acquireErr
	}
	if revertErr != nil {
		_ = credentials.Release()
		return nil, fmt.Errorf("end logged-on Windows user impersonation: %w", revertErr)
	}
	return credentials, nil
}

func interactiveUserToken() (windows.Token, error) {
	consoleSessionID := windows.WTSGetActiveConsoleSessionId()
	var sessionInfo *windows.WTS_SESSION_INFO
	var sessionCount uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessionInfo, &sessionCount); err != nil {
		if consoleSessionID == 0xffffffff {
			return 0, fmt.Errorf("enumerate Windows sessions: %w", err)
		}
		var token windows.Token
		if tokenErr := windows.WTSQueryUserToken(consoleSessionID, &token); tokenErr != nil {
			return 0, errors.Join(err, tokenErr)
		}
		return token, nil
	}
	if sessionInfo != nil {
		defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessionInfo)))
	}
	sessions := unsafe.Slice(sessionInfo, int(sessionCount))
	var failures []error
	for _, sessionID := range proxyAuthSessionIDs(consoleSessionID, sessions) {
		var token windows.Token
		if err := windows.WTSQueryUserToken(sessionID, &token); err == nil {
			return token, nil
		} else {
			failures = append(failures, fmt.Errorf("query Windows session %d user token: %w", sessionID, err))
		}
	}
	if len(failures) == 0 {
		return 0, errors.New("no logged-on Windows user session is available")
	}
	return 0, errors.Join(failures...)
}

func proxyAuthSessionIDs(consoleSessionID uint32, sessions []windows.WTS_SESSION_INFO) []uint32 {
	result := make([]uint32, 0, len(sessions))
	appendState := func(state uint32) {
		if consoleSessionID != 0xffffffff {
			for _, session := range sessions {
				if session.SessionID == consoleSessionID && session.State == state {
					result = append(result, session.SessionID)
					break
				}
			}
		}
		for _, session := range sessions {
			if session.SessionID != consoleSessionID && session.State == state {
				result = append(result, session.SessionID)
			}
		}
	}
	appendState(windows.WTSActive)
	appendState(windows.WTSDisconnected)
	return result
}

func (*windowsNTLMProxyAuthenticator) Scheme() string {
	return "NTLM"
}

func (a *windowsNTLMProxyAuthenticator) InitialToken() []byte {
	return a.initial
}

func (a *windowsNTLMProxyAuthenticator) NextToken(challenge []byte) ([]byte, error) {
	return a.context.Update(challenge)
}

func (a *windowsNTLMProxyAuthenticator) Close() error {
	if a.context != nil {
		_ = a.context.Release()
	}
	if a.credentials != nil {
		return a.credentials.Release()
	}
	return nil
}
