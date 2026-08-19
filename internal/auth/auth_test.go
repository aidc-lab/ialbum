package auth

import (
	"bytes"
	"testing"
)

func TestPasswordAndSealer(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "correct horse battery staple") {
		t.Fatal("correct password rejected")
	}
	if VerifyPassword(hash, "wrong password") {
		t.Fatal("wrong password accepted")
	}
	if _, err := HashPassword("short"); err == nil {
		t.Fatal("short password accepted")
	}

	key := bytes.Repeat([]byte{7}, 32)
	sealer, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := sealer.Seal([]byte("webdav-password"), "connection-1")
	if err != nil {
		t.Fatal(err)
	}
	plain, err := sealer.Open(ciphertext, nonce, "connection-1")
	if err != nil || string(plain) != "webdav-password" {
		t.Fatalf("round trip failed: %q, %v", plain, err)
	}
	if _, err := sealer.Open(ciphertext, nonce, "connection-2"); err == nil {
		t.Fatal("AAD mismatch should fail")
	}
}
