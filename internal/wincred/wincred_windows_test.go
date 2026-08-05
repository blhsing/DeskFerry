//go:build windows

package wincred

import (
	"fmt"
	"os"
	"testing"
)

func TestCredentialRoundTrip(t *testing.T) {
	for _, test := range []struct {
		credentialType   uint32
		passwordReadable bool
	}{
		{credentialType: TypeGeneric, passwordReadable: true},
		// Windows accepts domain-password credentials for SMB but does not
		// disclose their password blobs back to ordinary CredRead callers.
		{credentialType: TypeDomainPassword, passwordReadable: false},
	} {
		t.Run(fmt.Sprintf("type-%d", test.credentialType), func(t *testing.T) {
			target := fmt.Sprintf("deskferry-test-%d-%d", os.Getpid(), test.credentialType)
			defer Delete(target, test.credentialType)
			if err := Write(target, test.credentialType, `TEST\user`, "unit-test-password"); err != nil {
				t.Fatal(err)
			}
			user, password, err := Read(target, test.credentialType)
			if err != nil {
				t.Fatal(err)
			}
			if user != `TEST\user` || (test.passwordReadable && password != "unit-test-password") {
				t.Fatalf("credential round trip mismatch: user=%q password_length=%d", user, len(password))
			}
			if err := Delete(target, test.credentialType); err != nil {
				t.Fatal(err)
			}
		})
	}
}
