package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSecurityConfigurationAndSecrets(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	initialized, err := database.IsInitialized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initialized {
		t.Fatal("database should not be initialized")
	}

	config := SecurityConfig{Salt: []byte("salt-0123456789"), VerifierNonce: []byte("nonce"), VerifierCipher: []byte("ciphertext")}
	if err := database.InitializeSecurity(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	stored, err := database.SecurityConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Salt) != string(config.Salt) {
		t.Fatal("security salt was not preserved")
	}

	if err := database.PutSecret(context.Background(), "app:example", "token", []byte("nonce"), []byte("ciphertext")); err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := database.Secret(context.Background(), "app:example", "token")
	if err != nil {
		t.Fatal(err)
	}
	if string(nonce) != "nonce" || string(ciphertext) != "ciphertext" {
		t.Fatal("secret payload was not preserved")
	}
}
