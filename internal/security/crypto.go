package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"crypto/pbkdf2"
)

const (
	keyLength      = 32
	iterationCount = 600000
	verifier       = "minidock/master-key/verifier/v1"
)

func NewSalt() ([]byte, error) {
	salt := make([]byte, 32)
	_, err := io.ReadFull(rand.Reader, salt)
	return salt, err
}

func DeriveKey(password string, salt []byte) ([]byte, error) {
	if len(salt) < 16 {
		return nil, errors.New("salt is too short")
	}
	return pbkdf2.Key(sha256.New, password, salt, iterationCount, keyLength)
}

func NewVerifier(key []byte) (nonce, ciphertext []byte, err error) {
	return Encrypt(key, []byte(verifier))
}

func ValidateKey(key, nonce, ciphertext []byte) error {
	plaintext, err := Decrypt(key, nonce, ciphertext)
	if err != nil {
		return err
	}
	defer Zero(plaintext)
	if string(plaintext) != verifier {
		return errors.New("invalid master password")
	}
	return nil
}

func Encrypt(key, plaintext []byte) (nonce, ciphertext []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return nonce, gcm.Seal(nil, nonce, plaintext, nil), nil
}

func Decrypt(key, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func Zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
