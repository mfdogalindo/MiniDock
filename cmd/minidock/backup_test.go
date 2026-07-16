package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/julieta/minidock/internal/security"
	"github.com/julieta/minidock/internal/store"
)

func TestBackupVerifyRestoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.db")
	db, err := store.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	password := "backup test password"
	salt, _ := security.NewSalt()
	key, _ := security.DeriveKey(password, salt)
	nonce, cipher, _ := security.NewVerifier(key)
	security.Zero(key)
	if err := db.InitializeSecurity(context.Background(), store.SecurityConfig{Salt: salt, VerifierNonce: nonce, VerifierCipher: cipher}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	archive := filepath.Join(dir, "backup.mdbk")
	if err := backupCommand([]string{"backup", "--database", source, "--file", archive, "--password-stdin"}, bytes.NewBufferString(password)); err != nil {
		t.Fatal(err)
	}
	if err := backupCommand([]string{"verify", "--database", source, "--file", archive, "--password-stdin"}, bytes.NewBufferString(password)); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "restored", "minidock.db")
	if err := backupCommand([]string{"restore", "--database", source, "--file", archive, "--destination", destination, "--password-stdin"}, bytes.NewBufferString(password)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("restored mode = %o, want 0600", info.Mode().Perm())
	}
	restored, err := store.Open(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err := restored.IntegrityCheck(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFullBackupScriptsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.MkdirAll(filepath.Join(source, "apps"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "apps", "state.txt"), []byte("application data"), 0600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(source, "minidock.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	password := "script backup password"
	salt, _ := security.NewSalt()
	key, _ := security.DeriveKey(password, salt)
	nonce, cipher, _ := security.NewVerifier(key)
	security.Zero(key)
	if err := db.InitializeSecurity(context.Background(), store.SecurityConfig{Salt: salt, VerifierNonce: nonce, VerifierCipher: cipher}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	binary := filepath.Join(dir, "minidock")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = "."
	// Keep the caller's module cache: this helper must not force a network
	// download merely because the backup package gained an optional S3 client.
	build.Env = append(os.Environ(), "GOCACHE="+filepath.Join(dir, "go-cache"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v: %s", err, output)
	}
	backups := filepath.Join(dir, "backups")
	command := exec.Command("bash", "../../scripts/backup.sh")
	command.Dir = "."
	command.Env = append(os.Environ(),
		"MINIDOCK_DATA_PATH="+source,
		"MINIDOCK_DATABASE_PATH="+dbPath,
		"MINIDOCK_BACKUP_PATH="+backups,
		"MINIDOCK_BINARY="+binary,
	)
	command.Stdin = bytes.NewBufferString(password + "\n")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("backup script: %v: %s", err, output)
	}
	matches, err := filepath.Glob(filepath.Join(backups, "*.mdbk"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("backup archive = %v, %v", matches, err)
	}
	destination := filepath.Join(dir, "restored")
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	} // simulate a separate host/volume
	command = exec.Command("bash", "../../scripts/restore-backup.sh", matches[0], destination)
	command.Dir = "."
	command.Env = append(os.Environ(), "MINIDOCK_BINARY="+binary)
	command.Stdin = bytes.NewBufferString(password + "\n")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restore script: %v: %s", err, output)
	}
	data, err := os.ReadFile(filepath.Join(destination, "apps", "state.txt"))
	if err != nil || string(data) != "application data" {
		t.Fatalf("restored application data: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(destination, "minidock.db")); err != nil {
		t.Fatal(err)
	}
}
