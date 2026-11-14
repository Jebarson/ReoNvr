package security

import (
	"bytes"
	"testing"

	"reonavr/internal/testconfig"
)

func TestCredentialCipherRoundTrip(t *testing.T) {
	cipher, err := NewCredentialCipher(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}

	ciphertext, nonce, err := cipher.Encrypt("camera-one", testconfig.Password())
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := cipher.Decrypt("camera-one", ciphertext, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if plaintext != testconfig.Password() {
		t.Fatalf("unexpected plaintext %q", plaintext)
	}
}

func TestCredentialCipherUsesUniqueNonce(t *testing.T) {
	cipher, err := NewCredentialCipher(bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}

	first, firstNonce, err := cipher.Encrypt("camera-one", testconfig.Password())
	if err != nil {
		t.Fatal(err)
	}
	second, secondNonce, err := cipher.Encrypt("camera-one", testconfig.Password())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstNonce, secondNonce) || bytes.Equal(first, second) {
		t.Fatal("credential encryption reused output")
	}
}

func TestCredentialCipherBindsCameraID(t *testing.T) {
	cipher, err := NewCredentialCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}

	ciphertext, nonce, err := cipher.Encrypt("camera-one", testconfig.Password())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cipher.Decrypt("camera-two", ciphertext, nonce); err == nil {
		t.Fatal("expected associated-data authentication failure")
	}
}

func TestCredentialCipherRejectsInvalidKey(t *testing.T) {
	if _, err := NewCredentialCipher([]byte("too-short")); err == nil {
		t.Fatal("expected invalid key error")
	}
}
