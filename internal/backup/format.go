// Package backup implements MiniDock's versioned, authenticated database backup format.
package backup

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/julieta/minidock/internal/security"
)

var magic = [4]byte{'M', 'D', 'B', 'K'}

const (
	Version    byte = 1
	headerSize      = 14 // magic, version, nonce length, ciphertext length
)

// Seal returns a self-describing backup. Its header is authenticated by AES-GCM,
// so a modified version, nonce length or payload length is rejected on opening.
func Seal(key, database []byte) ([]byte, error) {
	if len(database) == 0 {
		return nil, errors.New("empty database backup")
	}
	nonceSize, overhead, err := security.GCMParameters(key)
	if err != nil {
		return nil, err
	}
	if nonceSize > 255 {
		return nil, errors.New("backup nonce too long")
	}
	header := make([]byte, headerSize)
	copy(header, magic[:])
	header[4] = Version
	header[5] = byte(nonceSize)
	binary.BigEndian.PutUint64(header[6:], uint64(len(database)+overhead))
	nonce, ciphertext, err := security.EncryptWithAAD(key, database, header)
	if err != nil {
		return nil, err
	}
	result := append(header, nonce...)
	return append(result, ciphertext...), nil
}

// Open authenticates and decodes one backup format version.
func Open(key, encoded []byte) ([]byte, error) {
	if len(encoded) < headerSize || string(encoded[:4]) != string(magic[:]) {
		return nil, errors.New("invalid MiniDock backup magic")
	}
	if encoded[4] != Version {
		return nil, fmt.Errorf("unsupported MiniDock backup format version %d", encoded[4])
	}
	nonceLen := int(encoded[5])
	ciphertextLen := binary.BigEndian.Uint64(encoded[6:14])
	prefixLen := headerSize + nonceLen
	if nonceLen == 0 || ciphertextLen == 0 || ciphertextLen > uint64(len(encoded)) || uint64(prefixLen)+ciphertextLen != uint64(len(encoded)) {
		return nil, errors.New("invalid MiniDock backup lengths")
	}
	nonce := encoded[headerSize : headerSize+nonceLen]
	ciphertext := encoded[headerSize+nonceLen:]
	return security.DecryptWithAAD(key, nonce, ciphertext, encoded[:headerSize])
}
