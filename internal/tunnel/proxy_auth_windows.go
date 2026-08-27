//go:build windows

package tunnel

import (
	"github.com/alexbrainman/sspi"
	"github.com/alexbrainman/sspi/ntlm"
)

type windowsNTLMProxyAuthenticator struct {
	credentials *sspi.Credentials
	context     *ntlm.ClientContext
	initial     []byte
}

func newIntegratedProxyAuthenticator() (integratedProxyAuthenticator, error) {
	credentials, err := ntlm.AcquireCurrentUserCredentials()
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
