package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("store: record not found")

type Store struct {
	db *sql.DB
}

type SecurityConfig struct {
	Salt           []byte
	VerifierNonce  []byte
	VerifierCipher []byte
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)

	store := &Store{db: database}
	if err := store.migrate(context.Background()); err != nil {
		database.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) IsInitialized(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settings WHERE key = 'security.salt'").Scan(&count)
	return count == 1, err
}

func (s *Store) InitializeSecurity(ctx context.Context, config SecurityConfig) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for key, value := range map[string][]byte{
		"security.salt":            config.Salt,
		"security.verifier_nonce":  config.VerifierNonce,
		"security.verifier_cipher": config.VerifierCipher,
	} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?)", key, value); err != nil {
			return fmt.Errorf("store %s: %w", key, err)
		}
	}
	return tx.Commit()
}

func (s *Store) SecurityConfig(ctx context.Context) (SecurityConfig, error) {
	values := make(map[string][]byte, 3)
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings WHERE key IN ('security.salt', 'security.verifier_nonce', 'security.verifier_cipher')")
	if err != nil {
		return SecurityConfig{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return SecurityConfig{}, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return SecurityConfig{}, err
	}
	if len(values) != 3 {
		return SecurityConfig{}, ErrNotFound
	}
	return SecurityConfig{
		Salt:           values["security.salt"],
		VerifierNonce:  values["security.verifier_nonce"],
		VerifierCipher: values["security.verifier_cipher"],
	}, nil
}

func (s *Store) PutSecret(ctx context.Context, scope, name string, nonce, ciphertext []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO secrets(scope, name, nonce, ciphertext, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(scope, name) DO UPDATE SET nonce = excluded.nonce, ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		scope, name, nonce, ciphertext, time.Now().UTC())
	return err
}

func (s *Store) Secret(ctx context.Context, scope, name string) (nonce, ciphertext []byte, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT nonce, ciphertext FROM secrets WHERE scope = ? AND name = ?", scope, name).Scan(&nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return nonce, ciphertext, err
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		PRAGMA foreign_keys = ON;
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value BLOB NOT NULL
		);
		CREATE TABLE IF NOT EXISTS secrets (
			scope TEXT NOT NULL,
			name TEXT NOT NULL,
			nonce BLOB NOT NULL,
			ciphertext BLOB NOT NULL,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (scope, name)
		);`)
	return err
}
