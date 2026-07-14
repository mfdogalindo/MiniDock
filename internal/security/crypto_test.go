package security

import "testing"

func TestDerivedKeyValidatesVerifier(t *testing.T) {
	salt, err := NewSalt()
	if err != nil {
		t.Fatal(err)
	}
	key, err := DeriveKey("a sufficiently long master password", salt)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(key)
	nonce, ciphertext, err := NewVerifier(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateKey(key, nonce, ciphertext); err != nil {
		t.Fatalf("expected a valid key: %v", err)
	}

	wrongKey, err := DeriveKey("another sufficiently long password", salt)
	if err != nil {
		t.Fatal(err)
	}
	defer Zero(wrongKey)
	if err := ValidateKey(wrongKey, nonce, ciphertext); err == nil {
		t.Fatal("expected wrong key to fail validation")
	}
}
