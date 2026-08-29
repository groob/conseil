package main

import "testing"

func TestValidateAuthenticatedBindAddress(t *testing.T) {
	t.Parallel()

	for _, address := range []string{"127.0.0.1:8000", "[::1]:8000", "localhost:8000"} {
		if err := validateBindAddress(address, false); err != nil {
			t.Errorf("validateBindAddress(%q, false) = %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:8000", "[::]:8000", "192.0.2.1:8000", ":8000"} {
		if err := validateBindAddress(address, false); err == nil {
			t.Errorf("validateBindAddress(%q, false) succeeded", address)
		}
	}
}

func TestValidateUnauthenticatedBindAddressAllowsNonLoopback(t *testing.T) {
	t.Parallel()

	if err := validateBindAddress("0.0.0.0:8000", true); err != nil {
		t.Fatal(err)
	}
}
