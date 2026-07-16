package backup

import (
	"bytes"
	"testing"
)

func TestSealOpenRoundTripAndAuthentication(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	plain := []byte("SQLite format 3\x00database")
	encoded, err := Seal(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Open(key, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, plain) {
		t.Fatal("backup did not round-trip")
	}
	encoded[4] ^= 1
	if _, err := Open(key, encoded); err == nil {
		t.Fatal("expected authenticated header failure")
	}
}

func TestS3ConfigRejectsMissingCredentials(t *testing.T) {
	if err := (S3Config{Bucket: "backups"}).Validate(); err == nil {
		t.Fatal("S3 configuration without credentials was accepted")
	}
	if err := (S3Config{Bucket: "backups", AccessKey: "key", SecretKey: "secret"}).Validate(); err != nil {
		t.Fatalf("valid S3 configuration rejected: %v", err)
	}
}
