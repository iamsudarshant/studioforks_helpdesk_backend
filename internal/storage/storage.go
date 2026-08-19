// Package storage persists uploaded files. Bodies are encrypted with a
// per-file data-encryption key sealed by the master KEK, and stored under a
// path that is never exposed through the API.
package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/platform"
)

// Object is the result of a successful write.
type Object struct {
	Path       string // relative to the storage root
	Size       int64
	Checksum   string
	Nonce      []byte
	WrappedDEK []byte
	KeyID      string
}

// Storage is the file-persistence contract. The local implementation ships
// today; an S3 implementation can be added without touching callers.
type Storage interface {
	// Put encrypts and writes a file, returning its stored location.
	Put(tenantSlug, category, filename string, r io.Reader) (*Object, error)
	// Get returns a reader over the decrypted body.
	Get(obj *Object) (io.ReadCloser, error)
	// Delete removes a stored file. A missing file is not an error.
	Delete(path string) error
	// Exists reports whether a stored file is present.
	Exists(path string) bool
	// Health verifies the backing store is writable.
	Health() error
}

// Local writes to the server filesystem outside the web root.
type Local struct {
	root     string
	sealer   *platform.Sealer
	maxBytes int64
	allowed  map[string]struct{}
}

func NewLocal(cfg config.Storage, sealer *platform.Sealer) (*Local, error) {
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("resolving storage root: %w", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("creating storage root: %w", err)
	}

	allowed := make(map[string]struct{}, len(cfg.AllowedExt))
	for _, ext := range cfg.AllowedExt {
		allowed[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))] = struct{}{}
	}

	return &Local{root: root, sealer: sealer, maxBytes: cfg.MaxUploadBytes, allowed: allowed}, nil
}

// AllowedExtension reports whether an extension is permitted. MIME sniffing is
// performed separately by the document service; both must pass.
func (l *Local) AllowedExtension(filename string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	_, ok := l.allowed[ext]
	return ok
}

func (l *Local) MaxBytes() int64 { return l.maxBytes }

// Put encrypts the body with a fresh DEK and writes it under
// <root>/<tenant>/<category>/<yyyy>/<mm>/<ulid><ext>.
func (l *Local) Put(tenantSlug, category, filename string, r io.Reader) (*Object, error) {
	if tenantSlug == "" {
		return nil, errors.New("tenant slug is required for storage isolation")
	}

	now := time.Now().UTC()
	ext := strings.ToLower(filepath.Ext(filename))
	name := platform.NewULID() + ext

	relDir := filepath.ToSlash(filepath.Join(
		sanitiseSegment(tenantSlug),
		sanitiseSegment(category),
		now.Format("2006"),
		now.Format("01"),
	))
	relPath := relDir + "/" + name

	absDir := filepath.Join(l.root, filepath.FromSlash(relDir))
	if err := os.MkdirAll(absDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating storage directory: %w", err)
	}

	// Read the whole body so the checksum and the AEAD tag cover the same
	// bytes. Uploads are capped well below memory limits by MaxBytes.
	body, err := io.ReadAll(io.LimitReader(r, l.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading upload: %w", err)
	}
	if int64(len(body)) > l.maxBytes {
		return nil, ErrTooLarge
	}

	sum := sha256.Sum256(body)

	dek, err := platform.NewDEK()
	if err != nil {
		return nil, err
	}
	aead, err := platform.StreamCipher(dek)
	if err != nil {
		return nil, err
	}
	nonce, err := platform.RandomBytes(aead.NonceSize())
	if err != nil {
		return nil, err
	}
	// The path is authenticated data, so a ciphertext moved to another path
	// fails to decrypt.
	ciphertext := aead.Seal(nil, nonce, body, []byte(relPath))

	wrapped, err := l.sealer.Seal(dek, []byte(tenantSlug))
	if err != nil {
		return nil, fmt.Errorf("wrapping data key: %w", err)
	}

	absPath := filepath.Join(l.root, filepath.FromSlash(relPath))
	// Write to a temporary file and rename, so a crash never leaves a partial
	// file that looks valid.
	tmp := absPath + ".tmp"
	if err := os.WriteFile(tmp, ciphertext, 0o640); err != nil {
		return nil, fmt.Errorf("writing file: %w", err)
	}
	if err := os.Rename(tmp, absPath); err != nil {
		_ = os.Remove(tmp)
		return nil, fmt.Errorf("finalising file: %w", err)
	}

	return &Object{
		Path:       relPath,
		Size:       int64(len(body)),
		Checksum:   hex.EncodeToString(sum[:]),
		Nonce:      nonce,
		WrappedDEK: wrapped,
		KeyID:      platform.NewULID(),
	}, nil
}

var (
	// ErrTooLarge is returned when an upload exceeds the configured limit.
	ErrTooLarge = errors.New("file exceeds the maximum allowed size")
	// ErrNotFound is returned when a stored file is missing.
	ErrNotFound = errors.New("stored file not found")
	// ErrCorrupt is returned when decryption fails, which means the file was
	// modified, moved, or encrypted under a different key.
	ErrCorrupt = errors.New("stored file failed integrity verification")
)

// Get decrypts and returns the file body.
func (l *Local) Get(obj *Object) (io.ReadCloser, error) {
	abs, err := l.resolve(obj.Path)
	if err != nil {
		return nil, err
	}

	ciphertext, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("reading file: %w", err)
	}

	dek, err := l.sealer.Open(obj.WrappedDEK, []byte(tenantFromPath(obj.Path)))
	if err != nil {
		return nil, fmt.Errorf("unwrapping data key: %w", err)
	}
	aead, err := platform.StreamCipher(dek)
	if err != nil {
		return nil, err
	}

	plaintext, err := aead.Open(nil, obj.Nonce, ciphertext, []byte(obj.Path))
	if err != nil {
		return nil, ErrCorrupt
	}
	return io.NopCloser(strings.NewReader(string(plaintext))), nil
}

func (l *Local) Delete(path string) error {
	abs, err := l.resolve(path)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting file: %w", err)
	}
	return nil
}

func (l *Local) Exists(path string) bool {
	abs, err := l.resolve(path)
	if err != nil {
		return false
	}
	_, err = os.Stat(abs)
	return err == nil
}

func (l *Local) Health() error {
	probe := filepath.Join(l.root, ".healthcheck")
	if err := os.WriteFile(probe, []byte("ok"), 0o640); err != nil {
		return fmt.Errorf("storage root is not writable: %w", err)
	}
	return os.Remove(probe)
}

// resolve turns a stored relative path into an absolute one, rejecting anything
// that would escape the storage root.
func (l *Local) resolve(rel string) (string, error) {
	if rel == "" {
		return "", ErrNotFound
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("invalid storage path %q", rel)
	}

	abs := filepath.Join(l.root, clean)
	// Belt and braces: confirm the joined path is still inside the root.
	if !strings.HasPrefix(abs, l.root+string(os.PathSeparator)) && abs != l.root {
		return "", fmt.Errorf("storage path escapes root: %q", rel)
	}
	return abs, nil
}

// sanitiseSegment keeps path segments to a safe alphabet so a tenant slug or
// category can never introduce a traversal.
func sanitiseSegment(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "misc"
	}
	if len(out) > 64 {
		return out[:64]
	}
	return out
}

func tenantFromPath(rel string) string {
	parts := strings.SplitN(rel, "/", 2)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}
