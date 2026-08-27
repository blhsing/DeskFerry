//go:build !windows

package tunnel

func newIntegratedProxyAuthenticator() (integratedProxyAuthenticator, error) {
	return nil, nil
}
