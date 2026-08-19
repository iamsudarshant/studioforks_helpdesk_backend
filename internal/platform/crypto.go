package platform

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// Sealer performs authenticated encryption with the master key-encryption key.
// It is used for two things: sealing per-file data-encryption keys, and sealing
// small secrets held in the database (SMTP passwords, TOTP secrets).
type Sealer struct {
	aead cipher.AEAD
}

func NewSealer(kek []byte) (*Sealer, error) {
	if len(kek) != 32 {
		return nil, fmt.Errorf("key-encryption key must be 32 bytes, got %d", len(kek))
	}
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, fmt.Errorf("creating cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("creating GCM: %w", err)
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext, prefixing the nonce. aad binds the ciphertext to a
// context (for example the tenant slug) so a blob cannot be replayed elsewhere.
func (s *Sealer) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generating nonce: %w", err)
	}
	return s.aead.Seal(nonce, nonce, plaintext, aad), nil
}

// Open reverses Seal. A modified ciphertext or a mismatched aad fails here.
func (s *Sealer) Open(sealed, aad []byte) ([]byte, error) {
	ns := s.aead.NonceSize()
	if len(sealed) < ns {
		return nil, errors.New("ciphertext is too short")
	}
	plaintext, err := s.aead.Open(nil, sealed[:ns], sealed[ns:], aad)
	if err != nil {
		return nil, fmt.Errorf("decrypting: %w", err)
	}
	return plaintext, nil
}

// SealString is a convenience wrapper returning base64 for text columns.
func (s *Sealer) SealString(plaintext string, aad []byte) (string, error) {
	sealed, err := s.Seal([]byte(plaintext), aad)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func (s *Sealer) OpenString(encoded string, aad []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding ciphertext: %w", err)
	}
	plain, err := s.Open(raw, aad)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// NewDEK generates a fresh 32-byte data-encryption key for one document.
func NewDEK() ([]byte, error) { return RandomBytes(32) }

// StreamCipher builds the AEAD used to encrypt a document body with its DEK.
func StreamCipher(dek []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(dek)
	if err != nil {
		return nil, fmt.Errorf("creating document cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// HashToken returns the hex SHA-256 of a token. Refresh tokens, reset tokens,
// OTPs and API keys are stored only as this hash, so a database leak does not
// hand over usable credentials.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// HMACSign produces a hex HMAC-SHA256, used for signed document URLs and for
// the audit hash chain.
func HMACSign(key []byte, message string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

// HMACVerify compares a signature in constant time.
func HMACVerify(key []byte, message, signature string) bool {
	expected := HMACSign(key, message)
	return hmac.Equal([]byte(expected), []byte(signature))
}

// SHA256Hex hashes arbitrary bytes, used for file checksums and request hashes.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
