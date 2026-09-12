package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const KeyVersion = 1

type CredentialCipher struct {
	aead cipher.AEAD
}

func LoadCredentialCipher(path string) (*CredentialCipher, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential key: %w", err)
	}

	key := encoded
	if len(key) != 32 {
		key, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(encoded)))
		if err != nil {
			return nil, errors.New("credential key must be 32 raw bytes or base64 encoded")
		}
	}
	return NewCredentialCipher(key)
}

func LoadOrCreateCredentialCipher(path string) (*CredentialCipher, error) {
	cipher, err := LoadCredentialCipher(path)
	if err == nil {
		return cipher, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create credential key directory: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate credential key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return LoadCredentialCipher(path)
	}
	if err != nil {
		return nil, fmt.Errorf("create credential key: %w", err)
	}
	if _, err := file.Write(key); err != nil {
		file.Close()
		return nil, fmt.Errorf("write credential key: %w", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return nil, fmt.Errorf("sync credential key: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close credential key: %w", err)
	}
	return NewCredentialCipher(key)
}

func NewCredentialCipher(key []byte) (*CredentialCipher, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("credential key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create credential gcm: %w", err)
	}
	return &CredentialCipher{aead: aead}, nil
}

func (cipher *CredentialCipher) Encrypt(cameraID, password string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, cipher.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate credential nonce: %w", err)
	}
	ciphertext = cipher.aead.Seal(nil, nonce, []byte(password), associatedData(cameraID))
	return ciphertext, nonce, nil
}

func (cipher *CredentialCipher) Decrypt(cameraID string, ciphertext, nonce []byte) (string, error) {
	plaintext, err := cipher.aead.Open(nil, nonce, ciphertext, associatedData(cameraID))
	if err != nil {
		return "", errors.New("decrypt credential: authentication failed")
	}
	return string(plaintext), nil
}

func associatedData(cameraID string) []byte {
	return []byte(fmt.Sprintf("ReoNvr:Camera:%s:Credential:%d", cameraID, KeyVersion))
}