package platform

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// NewULID returns a lexicographically sortable 26-character public identifier.
// Every table exposes one of these; numeric primary keys never leave the
// process, so ids cannot be enumerated across tenants.
func NewULID() string {
	return ulid.MustNew(ulid.Timestamp(time.Now().UTC()), rand.Reader).String()
}

// ValidULID reports whether s is a syntactically valid public id. Handlers use
// it to reject malformed path parameters before touching the database.
func ValidULID(s string) bool {
	if len(s) != 26 {
		return false
	}
	_, err := ulid.ParseStrict(s)
	return err == nil
}

// RandomBytes returns n cryptographically random bytes.
func RandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("reading random bytes: %w", err)
	}
	return b, nil
}

// RandomToken returns a URL-safe random token carrying n bytes of entropy.
// Used for refresh tokens, reset links and signed-URL nonces.
func RandomToken(n int) (string, error) {
	b, err := RandomBytes(n)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// unambiguous omits I, l, 1, O and 0 so temporary passwords survive being read
// off a screen or dictated over the phone.
const unambiguous = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789"

// RandomPassword generates a temporary password of the requested length from an
// unambiguous alphabet, guaranteeing at least one upper, one lower and one digit.
func RandomPassword(length int) (string, error) {
	if length < 8 {
		length = 8
	}
	out := make([]byte, length)
	for i := range out {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(unambiguous))))
		if err != nil {
			return "", fmt.Errorf("generating password: %w", err)
		}
		out[i] = unambiguous[idx.Int64()]
	}

	pw := string(out)
	if !strings.ContainsAny(pw, "ABCDEFGHJKLMNPQRSTUVWXYZ") ||
		!strings.ContainsAny(pw, "abcdefghijkmnopqrstuvwxyz") ||
		!strings.ContainsAny(pw, "23456789") {
		// Astronomically rare; regenerate rather than patch in a fixed position.
		return RandomPassword(length)
	}
	return pw, nil
}

// NumericCode returns a zero-padded numeric OTP of the given digit count.
func NumericCode(digits int) (string, error) {
	if digits <= 0 {
		digits = 6
	}
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(digits)), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "", fmt.Errorf("generating code: %w", err)
	}
	return fmt.Sprintf("%0*d", digits, n), nil
}

// Base32Secret returns an unpadded base32 secret suitable for TOTP enrolment.
func Base32Secret(n int) (string, error) {
	b, err := RandomBytes(n)
	if err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b), nil
}

// HexEncode is a small helper so callers do not import encoding/hex directly
// alongside this package.
func HexEncode(b []byte) string { return hex.EncodeToString(b) }

// ConstantTimeEqual compares two strings without leaking length-independent
// timing information. Use it for every token/hash comparison.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
