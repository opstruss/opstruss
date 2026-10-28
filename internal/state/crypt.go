package state

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the size of the data key of a stack, for AES-256.
const KeySize = 32

// sealed starts a value that Seal encrypted, with the version of its form.
const sealed = "enc:v1:"

// IsSealed reports whether s is a value that Seal encrypted.
func IsSealed(s string) bool { return strings.HasPrefix(s, sealed) }

// Seal encrypts v, as JSON, with AES-256-GCM under the data key, bound to
// aad, such as the id of its record, so it cannot be moved to another.
func Seal(key []byte, aad string, v any) (string, error) {
	plain, err := encodeJSON(floats(v))
	if err != nil {
		return "", err
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return sealed + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, []byte(aad))), nil
}

// Unseal decrypts what Seal made with the same key and aad.
func Unseal(key []byte, aad, s string) (any, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, sealed))
	if err != nil || !IsSealed(s) {
		return nil, errors.New("not a value that OpsTruss encrypted")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, errors.New("an encrypted value too short to be one")
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte(aad))
	if err != nil {
		return nil, errors.New("an encrypted value that this data key does not open, or that belongs elsewhere")
	}
	var v any
	if err := decodeJSON(plain, &v); err != nil {
		return nil, err
	}
	return fixNumbers(v), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("a data key has %d bytes, not %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// DigestKey derives from the data key, through HKDF, the key that hashes
// the sensitive values in the digest of the record with id, so a digest
// changes when a secret does but tells nothing about it.
func DigestKey(key []byte, id string) ([]byte, error) {
	return hkdf.Key(sha256.New, key, []byte(id), "opstruss digest", KeySize)
}
