package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	backupformat "github.com/julieta/minidock/internal/backup"
	"github.com/julieta/minidock/internal/security"
	"github.com/julieta/minidock/internal/store"
)

func backupCommand(args []string, stdin io.Reader) error {
	if len(args) == 0 {
		return errors.New("usage: minidock backup|verify|restore|seal|open --database PATH --file PATH --password-stdin")
	}
	fs := flag.NewFlagSet("minidock "+args[0], flag.ContinueOnError)
	keyDatabase := fs.String("database", "./data/minidock.db", "existing MiniDock SQLite database used to unlock the KMS")
	kmsConfig := fs.String("kms-config", "", "portable non-secret KMS configuration JSON")
	destination := keyDatabase
	file := fs.String("file", "", "backup .mdbk file")
	input := fs.String("input", "", "plaintext input for seal")
	output := fs.String("output", "", "plaintext output for open")
	if args[0] == "kms-export" {
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *file == "" {
			return errors.New("--file is required for kms-export")
		}
		return exportKMSConfig(*keyDatabase, *file)
	}
	passwordStdin := fs.Bool("password-stdin", false, "read KMS password from standard input")
	if args[0] == "restore" {
		destination = fs.String("destination", "", "new SQLite destination (must not exist)")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *file == "" || *keyDatabase == "" || *destination == "" || !*passwordStdin {
		return errors.New("--file, --database, database/destination and --password-stdin are required")
	}
	password, err := io.ReadAll(stdin)
	if err != nil {
		return fmt.Errorf("read password: %w", err)
	}
	defer security.Zero(password)
	password = []byte(strings.TrimSpace(string(password)))
	if len(password) == 0 {
		return errors.New("empty KMS password")
	}

	if args[0] == "backup" {
		return createBackup(*keyDatabase, *file, password)
	}
	if args[0] == "seal" {
		if *input == "" {
			return errors.New("--input is required for seal")
		}
		return sealFile(*keyDatabase, *kmsConfig, *input, *file, password)
	}
	encoded, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("read backup: %w", err)
	}
	key, err := backupKey(*keyDatabase, *kmsConfig, password)
	if err != nil {
		return err
	}
	defer security.Zero(key)
	plain, err := backupformat.Open(key, encoded)
	if err != nil {
		return fmt.Errorf("authenticate backup: %w", err)
	}
	defer security.Zero(plain)
	if args[0] == "open" {
		if *output == "" {
			return errors.New("--output is required for open")
		}
		if err := os.WriteFile(*output, plain, 0600); err != nil {
			return fmt.Errorf("write decrypted file: %w", err)
		}
		return nil
	}
	if err := verifySQLite(plain); err != nil {
		return err
	}
	if args[0] == "verify" {
		return nil
	}
	if args[0] != "restore" {
		return errors.New("unknown backup command")
	}
	if _, err := os.Stat(*destination); err == nil {
		return errors.New("restore destination already exists")
	}
	if err := os.MkdirAll(filepath.Dir(*destination), 0700); err != nil {
		return err
	}
	if err := os.WriteFile(*destination, plain, 0600); err != nil {
		return fmt.Errorf("write restored database: %w", err)
	}
	return nil
}

func sealFile(databasePath, configPath, input, destination string, password []byte) error {
	key, err := backupKey(databasePath, configPath, password)
	if err != nil {
		return err
	}
	defer security.Zero(key)
	plain, err := os.ReadFile(input)
	if err != nil {
		return fmt.Errorf("read input: %w", err)
	}
	defer security.Zero(plain)
	encoded, err := backupformat.Seal(key, plain)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, encoded, 0600)
}

func createBackup(databasePath, destination string, password []byte) error {
	key, err := backupKey(databasePath, "", password)
	if err != nil {
		return err
	}
	defer security.Zero(key)
	db, err := store.Open(databasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".minidock-backup-*.db")
	if err != nil {
		return err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := db.Backup(context.Background(), tmp.Name()); err != nil {
		return err
	}
	plain, err := os.ReadFile(tmp.Name())
	if err != nil {
		return err
	}
	defer security.Zero(plain)
	encoded, err := backupformat.Seal(key, plain)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, encoded, 0600)
}

func backupKey(databasePath, configPath string, password []byte) ([]byte, error) {
	var cfg store.SecurityConfig
	var err error
	if configPath != "" {
		data, readErr := os.ReadFile(configPath)
		if readErr != nil {
			return nil, readErr
		}
		if err = json.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("read KMS config: %w", err)
		}
	} else {
		db, openErr := store.Open(databasePath)
		if openErr != nil {
			return nil, openErr
		}
		defer db.Close()
		cfg, err = db.SecurityConfig(context.Background())
		if err != nil {
			return nil, err
		}
	}
	key, err := security.DeriveKey(string(password), cfg.Salt)
	if err != nil {
		return nil, err
	}
	if err := security.ValidateKey(key, cfg.VerifierNonce, cfg.VerifierCipher); err != nil {
		security.Zero(key)
		return nil, errors.New("KMS password rejected")
	}
	return key, nil
}

func exportKMSConfig(databasePath, destination string) error {
	db, err := store.Open(databasePath)
	if err != nil {
		return err
	}
	defer db.Close()
	cfg, err := db.SecurityConfig(context.Background())
	if err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0600)
}

func verifySQLite(plain []byte) error {
	dir, err := os.MkdirTemp("", "minidock-restore-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "minidock.db")
	if err := os.WriteFile(path, plain, 0600); err != nil {
		return err
	}
	db, err := store.Open(path)
	if err != nil {
		return fmt.Errorf("open restored database: %w", err)
	}
	defer db.Close()
	if err := db.IntegrityCheck(context.Background()); err != nil {
		return err
	}
	return nil
}
