// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SecretEnv, when set, derives the at-rest key instead of the generated
// key file (useful when the data volume is not persistent or is shared).
const SecretEnv = "TRACKER_SECRET"

// keyFile is the generated key's name inside the data directory.
const keyFile = "secret.key"

// keyInfo binds derived keys to this use.
const keyInfo = "corruption-tracker settings v1"

// ErrUndecryptable means a stored secret was sealed with a different key
// (secret.key lost or TRACKER_SECRET changed): the operator re-enters it.
var ErrUndecryptable = errors.New("stored secret cannot be decrypted with this instance's key; re-enter it")

// sealer encrypts secrets with AES-256-GCM. Ciphertext layout:
// nonce (12 bytes) || sealed. The setting name is the additional data, so
// a ciphertext cannot be moved to another key.
type sealer struct{ aead cipher.AEAD }

// loadKey derives the key from TRACKER_SECRET, or reads (creating on first
// boot) dataDir/secret.key.
func loadKey(lookupEnv func(string) (string, bool), dataDir string) ([]byte, error) {
	if v, ok := lookupEnv(SecretEnv); ok {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("%s is set but blank", SecretEnv)
		}
		return hkdf.Key(sha256.New, []byte(v), nil, keyInfo, 32)
	}
	if dataDir == "" {
		return nil, errors.New("settings: no data directory for the secret key (set TRACKER_DATA_DIR or TRACKER_SECRET)")
	}
	path := filepath.Join(dataDir, keyFile)
	b, err := os.ReadFile(path)
	if err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(b)))
		if derr != nil || len(key) != 32 {
			return nil, fmt.Errorf("settings: %s is not a 32-byte hex key", path)
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("settings: read the secret key in the data directory (TRACKER_DATA_DIR): %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("settings: create data directory: %w", err)
	}
	// O_EXCL: two processes booting together must not overwrite each
	// other's key; the loser re-reads the winner's.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return loadKey(lookupEnv, dataDir)
	}
	if err != nil {
		return nil, fmt.Errorf("settings: create %s: %w", path, err)
	}
	if _, err := f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return key, nil
}

func newSealer(key []byte) (*sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &sealer{aead: aead}, nil
}

func (s *sealer) seal(name, plaintext string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(plaintext), []byte(name)), nil
}

func (s *sealer) open(name string, ciphertext []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(ciphertext) < n {
		return "", ErrUndecryptable
	}
	pt, err := s.aead.Open(nil, ciphertext[:n], ciphertext[n:], []byte(name))
	if err != nil {
		return "", ErrUndecryptable
	}
	return string(pt), nil
}
