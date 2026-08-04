//go:build windows

package wincred

import (
	"fmt"
	"os"
	"testing"
)

func TestGenericCredentialRoundTrip(t *testing.T) {
	target := fmt.Sprintf("DeskFerry/test/%d", os.Getpid())
	defer Delete(target, TypeGeneric)
	if err := Write(target, TypeGeneric, `TEST\user`, "unit-test-password"); err != nil {
		t.Fatal(err)
	}
	user, password, err := Read(target, TypeGeneric)
	if err != nil {
		t.Fatal(err)
	}
	if user != `TEST\user` || password != "unit-test-password" {
		t.Fatalf("credential round trip mismatch: user=%q password_length=%d", user, len(password))
	}
	if err := Delete(target, TypeGeneric); err != nil {
		t.Fatal(err)
	}
}
