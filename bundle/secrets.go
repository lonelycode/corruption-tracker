// SPDX-License-Identifier: AGPL-3.0-only
// SPDX-FileCopyrightText: 2026 Martin Buhr and contributors

package bundle

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
)

// PassphraseEnv supplies the --include-secrets passphrase non-interactively.
const PassphraseEnv = "TRACKER_EXPORT_PASSPHRASE"

// MinPassphraseLen guards against a trivially guessable bundle passphrase:
// the bundle travels between operators.
const MinPassphraseLen = 12

// SecretOpener decrypts a stored secret setting (the settings store's
// at-rest ciphertext) so the exporter can re-seal it under the bundle
// passphrase.
type SecretOpener interface {
	OpenSecret(name string, ciphertext []byte) (string, error)
}

// secretsFile is secrets.enc (§10.7): scrypt-derived AES-256-GCM over the
// JSON object {setting: plaintext}.
type secretsFile struct {
	KDF        string `json:"kdf"`
	N          int    `json:"N"`
	R          int    `json:"r"`
	P          int    `json:"p"`
	Salt       string `json:"salt"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

const (
	scryptN = 32768
	scryptR = 8
	scryptP = 1
)

// ErrBadPassphrase means secrets.enc did not decrypt (wrong passphrase or
// a corrupted file).
var ErrBadPassphrase = errors.New("bundle: secrets.enc did not decrypt: wrong passphrase or corrupted file")

func checkPassphrase(p string) error {
	if utf8.RuneCountInString(p) < MinPassphraseLen {
		return fmt.Errorf("bundle: the secrets passphrase must be at least %d characters (set %s)", MinPassphraseLen, PassphraseEnv)
	}
	return nil
}

// sealSecrets encrypts secrets under passphrase into secrets.enc bytes.
func sealSecrets(secrets map[string]string, passphrase string) ([]byte, error) {
	if err := checkPassphrase(passphrase); err != nil {
		return nil, err
	}
	plain, err := json.Marshal(secrets)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	aead, err := passphraseAEAD(passphrase, salt, scryptN, scryptR, scryptP)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ct := aead.Seal(nil, nonce, plain, []byte(SecretsFile))
	f := secretsFile{KDF: "scrypt", N: scryptN, R: scryptR, P: scryptP,
		Salt:       base64.StdEncoding.EncodeToString(salt),
		Nonce:      base64.StdEncoding.EncodeToString(nonce),
		Ciphertext: base64.StdEncoding.EncodeToString(ct)}
	out, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// OpenSecrets decrypts secrets.enc bytes (for the importer).
func OpenSecrets(data []byte, passphrase string) (map[string]string, error) {
	var f secretsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("bundle: secrets.enc: %w", err)
	}
	if f.KDF != "scrypt" || f.N < 16384 || f.R < 1 || f.P < 1 {
		return nil, fmt.Errorf("bundle: secrets.enc: unsupported kdf %q (N=%d r=%d p=%d)", f.KDF, f.N, f.R, f.P)
	}
	salt, err1 := base64.StdEncoding.DecodeString(f.Salt)
	nonce, err2 := base64.StdEncoding.DecodeString(f.Nonce)
	ct, err3 := base64.StdEncoding.DecodeString(f.Ciphertext)
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, fmt.Errorf("bundle: secrets.enc: %w", err)
	}
	aead, err := passphraseAEAD(passphrase, salt, f.N, f.R, f.P)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, ErrBadPassphrase
	}
	plain, err := aead.Open(nil, nonce, ct, []byte(SecretsFile))
	if err != nil {
		return nil, ErrBadPassphrase
	}
	out := map[string]string{}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, fmt.Errorf("bundle: secrets.enc payload: %w", err)
	}
	return out, nil
}

func passphraseAEAD(passphrase string, salt []byte, n, r, p int) (cipher.AEAD, error) {
	key, err := scrypt.Key([]byte(passphrase), salt, n, r, p, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
