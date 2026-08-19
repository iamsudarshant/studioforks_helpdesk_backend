package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/config"
	"github.com/karmamgmt/complydesk/internal/httpx"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/storage"
)

type Service struct {
	repo  *Repository
	store *storage.Local
	cfg   *config.Config
}

func NewService(repo *Repository, store *storage.Local, cfg *config.Config) *Service {
	return &Service{repo: repo, store: store, cfg: cfg}
}

// UploadBrandLogo implements tenant.LogoUploader. A client logo is a TENANT-
// owned document so it lives under the client's storage path and its record
// travels with the workspace, but it is only ever referenced from branding.
func (s *Service) UploadBrandLogo(ctx context.Context, tenantSlug string, tenantID, uploaderID int64,
	header *multipart.FileHeader, file multipart.File) (string, error) {

	uploader := uploaderID
	doc, err := s.Upload(ctx, UploadParams{
		TenantSlug:  tenantSlug,
		TenantID:    tenantID,
		OwnerType:   "TENANT",
		OwnerID:     &tenantID,
		Description: "Client logo",
		UploadedBy:  &uploader,
		Header:      header,
		File:        file,
	})
	if err != nil {
		return "", err
	}
	return doc.PublicID, nil
}

// DiscardBrandLogo implements tenant.LogoUploader. It soft-deletes the document
// behind a replaced logo, so the record leaves the client's file list without
// breaking the stored bytes that a cached <img> may still be loading.
func (s *Service) DiscardBrandLogo(ctx context.Context, publicID string) error {
	doc, err := s.repo.ByPublicIDAcrossTenants(ctx, publicID)
	if err != nil {
		return nil // nothing to remove
	}
	return s.repo.SoftDelete(ctx, doc.TenantID, doc.ID)
}

func (s *Service) Repo() *Repository { return s.repo }

// allowedMIME maps an extension to the content types genuinely acceptable for
// it. The uploaded bytes are sniffed and checked against this, so renaming
// payload.exe to payload.pdf does not get it stored.
var allowedMIME = map[string][]string{
	"pdf":  {"application/pdf"},
	"jpg":  {"image/jpeg"},
	"jpeg": {"image/jpeg"},
	"png":  {"image/png"},
	"gif":  {"image/gif"},
	"webp": {"image/webp"},
	"txt":  {"text/plain"},
	"csv":  {"text/plain", "text/csv"},
	"doc":  {"application/msword", "application/x-ole-storage"},
	"docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/zip"},
	"xls":  {"application/vnd.ms-excel", "application/x-ole-storage"},
	"xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/zip"},
	"zip":  {"application/zip"},
	"svg":  {"image/svg+xml", "text/xml", "text/plain"},

	// The rest of the formats a compliance desk is actually sent: slide decks,
	// scanned images, OpenDocument from anyone not on Office, and RTF.
	"bmp":  {"image/bmp", "image/x-ms-bmp"},
	"tif":  {"image/tiff"},
	"tiff": {"image/tiff"},
	"heic": {"image/heic", "image/heif", "application/octet-stream"},
	"ppt":  {"application/vnd.ms-powerpoint", "application/x-ole-storage"},
	"pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation", "application/zip"},
	"odt":  {"application/vnd.oasis.opendocument.text", "application/zip"},
	"ods":  {"application/vnd.oasis.opendocument.spreadsheet", "application/zip"},
	"odp":  {"application/vnd.oasis.opendocument.presentation", "application/zip"},
	"rtf":  {"application/rtf", "text/rtf", "text/plain"},
}

// previewableInline are the types a browser can render directly. Everything
// else needs server-side conversion or a download.
var previewableInline = map[string]bool{
	"application/pdf": true,
	"image/jpeg":      true,
	"image/png":       true,
	"image/gif":       true,
	"image/webp":      true,
	"image/bmp":       true,
	"text/plain":      true,
}

type UploadParams struct {
	TenantSlug  string
	TenantID    int64
	OwnerType   string
	OwnerID     *int64
	CategoryID  *int64
	Description string
	UploadedBy  *int64
	Header      *multipart.FileHeader
	File        multipart.File
}

// StoreParams describes bytes to be validated and written, with no opinion
// about what record will reference them.
type StoreParams struct {
	TenantSlug string
	OwnerType  string
	Header     *multipart.FileHeader
	File       multipart.File
}

// Upload validates, encrypts and stores one file, and records it as a new
// document.
func (s *Service) Upload(ctx context.Context, p UploadParams) (*Document, error) {
	obj, filename, stored, err := s.Store(StoreParams{
		TenantSlug: p.TenantSlug, OwnerType: p.OwnerType,
		Header: p.Header, File: p.File,
	})
	if err != nil {
		return nil, err
	}

	doc, err := s.repo.Create(ctx, CreateParams{
		TenantID:     p.TenantID,
		OriginalName: filename,
		MimeType:     stored,
		Object:       obj,
		CategoryID:   p.CategoryID,
		Description:  p.Description,
		OwnerType:    p.OwnerType,
		OwnerID:      p.OwnerID,
		UploadedBy:   p.UploadedBy,
	})
	if err != nil {
		// The bytes are already on disk; remove them so a failed insert does
		// not leave an orphan file.
		s.Discard(obj)
		return nil, httpx.ErrInternal(err)
	}
	return doc, nil
}

// Discard removes stored bytes that no record ended up referencing.
func (s *Service) Discard(obj *storage.Object) {
	if obj != nil {
		_ = s.store.Delete(obj.Path)
	}
}

// Store validates and encrypts one uploaded file, returning the stored object
// along with the filename and content type that should be recorded for it.
//
// Every path that accepts bytes from a browser goes through here — a first
// upload and a new version alike — so extension checking, size limits and MIME
// sniffing cannot drift apart between them.
func (s *Service) Store(p StoreParams) (*storage.Object, string, string, error) {
	filename := filepath.Base(p.Header.Filename)
	if filename == "" || filename == "." || filename == string(filepath.Separator) {
		return nil, "", "", httpx.ErrField("file", "INVALID", "The uploaded file has no usable name.")
	}

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	if ext == "" {
		return nil, "", "", httpx.New(httpx.CodeUnsupportedMediaType,
			"Files must have a recognised extension.")
	}
	if !s.store.AllowedExtension(filename) {
		return nil, "", "", httpx.New(httpx.CodeUnsupportedMediaType,
			fmt.Sprintf("%s files are not accepted. Allowed types: %s.",
				strings.ToUpper(ext), strings.Join(s.cfg.Storage.AllowedExt, ", ")))
	}
	if p.Header.Size > s.store.MaxBytes() {
		return nil, "", "", httpx.New(httpx.CodePayloadTooLarge,
			fmt.Sprintf("This file is larger than the %d MB limit.", s.store.MaxBytes()/(1024*1024)))
	}

	// Sniff the head of the file to determine its real content type, then
	// rewind. 1 KB rather than the 512 bytes DetectContentType reads, because
	// a PDF may declare itself anywhere in that window.
	head := make([]byte, pdfSearchWindow)
	n, err := io.ReadFull(p.File, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, "", "", httpx.ErrInternal(fmt.Errorf("reading upload: %w", err))
	}
	head = head[:n]

	if _, err := p.File.Seek(0, io.SeekStart); err != nil {
		return nil, "", "", httpx.ErrInternal(fmt.Errorf("rewinding upload: %w", err))
	}

	detected := http.DetectContentType(head)
	if i := strings.Index(detected, ";"); i > 0 {
		detected = strings.TrimSpace(detected[:i])
	}
	if !mimeMatches(ext, detected, head) {
		return nil, "", "", httpx.New(httpx.CodeUnsupportedMediaType,
			fmt.Sprintf("This file's contents do not match its .%s extension.", ext))
	}

	// Office formats are ZIP containers, so the sniffed type is generic; record
	// the canonical type from the extension instead.
	stored := canonicalMIME(ext, detected)

	obj, err := s.store.Put(p.TenantSlug, strings.ToLower(p.OwnerType), filename, p.File)
	if err != nil {
		if errors.Is(err, storage.ErrTooLarge) {
			return nil, "", "", httpx.New(httpx.CodePayloadTooLarge,
				fmt.Sprintf("This file is larger than the %d MB limit.", s.store.MaxBytes()/(1024*1024)))
		}
		return nil, "", "", httpx.ErrInternal(err)
	}

	return obj, filename, stored, nil
}

// magicNumbers are the leading bytes that identify a format regardless of what
// it is called.
//
// http.DetectContentType answers "application/octet-stream" for anything it
// does not recognise, which includes both a legitimate Word document and a
// Windows executable. Accepting that answer on the strength of the extension
// alone is how payload.exe becomes payload.pdf, so where a format has a known
// signature it is checked here instead.
var magicNumbers = map[string][][]byte{
	"zip":  {{'P', 'K', 3, 4}, {'P', 'K', 5, 6}, {'P', 'K', 7, 8}},
	"docx": {{'P', 'K', 3, 4}},
	"xlsx": {{'P', 'K', 3, 4}},
	"pptx": {{'P', 'K', 3, 4}},
	// OpenDocument is a zip too.
	"odt": {{'P', 'K', 3, 4}},
	"ods": {{'P', 'K', 3, 4}},
	"odp": {{'P', 'K', 3, 4}},
	// The OLE compound-file header, shared by the pre-2007 Office formats.
	"doc": {{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}},
	"xls": {{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}},
	"ppt": {{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}},
	"rtf": {[]byte(`{\rtf`)},
	"bmp": {{'B', 'M'}},
	// Little- and big-endian TIFF.
	"tiff": {{'I', 'I', 42, 0}, {'M', 'M', 0, 42}},
	"tif":  {{'I', 'I', 42, 0}, {'M', 'M', 0, 42}},
}

// pdfHeader is where a PDF says what it is. The specification allows it
// anywhere in the first 1024 bytes rather than at byte zero, and real files use
// that latitude: a UTF-8 byte-order mark, leading whitespace, or bytes left by
// a signing or concatenation step all push it along. Requiring it at offset 0
// rejected such files as forgeries — the message read "this file's contents do
// not match its .pdf extension" about a PDF that opens perfectly well.
//
// Searching the window instead still catches the case this check exists for:
// an executable renamed to .pdf contains no such marker anywhere near its head.
const pdfSearchWindow = 1024

var pdfHeader = []byte("%PDF-")

func mimeMatches(ext, detected string, head []byte) bool {
	allowed, known := allowedMIME[ext]
	if !known {
		return false
	}

	if ext == "pdf" {
		window := head
		if len(window) > pdfSearchWindow {
			window = window[:pdfSearchWindow]
		}
		return bytes.Contains(window, pdfHeader)
	}

	// Where the format announces itself at a fixed offset, that is the answer —
	// a matching signature is proof, and a mismatched one is a rename. Zip and
	// OLE containers genuinely must start with theirs: a reader that cannot
	// find it at byte zero will not open the file either.
	if signatures, ok := magicNumbers[ext]; ok {
		for _, sig := range signatures {
			if bytes.HasPrefix(head, sig) {
				return true
			}
		}
		return false
	}

	for _, candidate := range allowed {
		if candidate == detected {
			return true
		}
	}
	return false
}

func canonicalMIME(ext, detected string) string {
	if byExt := mime.TypeByExtension("." + ext); byExt != "" {
		if i := strings.Index(byExt, ";"); i > 0 {
			byExt = strings.TrimSpace(byExt[:i])
		}
		return byExt
	}
	return detected
}

// Open returns the decrypted body of a document.
func (s *Service) Open(ctx context.Context, tenantID int64, doc *Document) (io.ReadCloser, error) {
	if !doc.EncryptionKeyID.Valid {
		return nil, httpx.ErrInternal(fmt.Errorf("document %s has no encryption key", doc.PublicID))
	}

	wrapped, err := s.repo.WrappedKey(ctx, doc.EncryptionKeyID.String)
	if err != nil {
		return nil, httpx.ErrInternal(err)
	}

	body, err := s.store.Get(&storage.Object{
		Path:       doc.StoredPath,
		Nonce:      doc.Nonce,
		WrappedDEK: wrapped,
	})
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrNotFound):
			return nil, httpx.ErrNotFound("That file")
		case errors.Is(err, storage.ErrCorrupt):
			// Integrity failure is a security event, not a routine 404.
			return nil, httpx.ErrInternal(fmt.Errorf("integrity check failed for document %s", doc.PublicID))
		default:
			return nil, httpx.ErrInternal(err)
		}
	}
	return body, nil
}

// ReadAll returns the whole decrypted body. Used for previews and small files.
func (s *Service) ReadAll(ctx context.Context, tenantID int64, doc *Document) ([]byte, error) {
	body, err := s.Open(ctx, tenantID, doc)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, body); err != nil {
		return nil, httpx.ErrInternal(err)
	}
	return buf.Bytes(), nil
}

// CanPreviewInline reports whether the browser can render this type directly.
func CanPreviewInline(mimeType string) bool { return previewableInline[mimeType] }

// --- signed URLs ------------------------------------------------------------

// SignedURL issues a short-lived, single-purpose link so an <img> or pdf.js
// viewer can load a file without putting a bearer token in a URL.
//
// The signature covers the document, action, version, viewer and expiry, so a
// link cannot be edited to reach a different file or an earlier revision of the
// same one, and cannot outlive its window.
func (s *Service) SignedURL(doc *Document, action string, version int, viewerID int64, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = s.cfg.Storage.SignedURLTTL
	}
	expires := time.Now().UTC().Add(ttl).Unix()

	action = strings.ToLower(action)
	signature := platform.HMACSign([]byte(s.cfg.Auth.JWTSecret),
		signedPayload(doc.PublicID, action, version, viewerID, expires))

	// Under /public because the link has to work without a session header —
	// that is the whole point of signing it.
	url := fmt.Sprintf("/api/v1/public/documents/%s/%s?expires=%d&viewer=%d&signature=%s",
		doc.PublicID, action, expires, viewerID, signature)
	if version > 0 {
		url += fmt.Sprintf("&version=%d", version)
	}
	return url, nil
}

// VerifySignedURL validates a signed link.
func (s *Service) VerifySignedURL(docPublicID, action string, version int, viewerID, expires int64, signature string) error {
	if time.Now().UTC().Unix() > expires {
		return httpx.ErrForbidden("This link has expired. Reopen the document to get a new one.")
	}

	payload := signedPayload(docPublicID, action, version, viewerID, expires)
	if !platform.HMACVerify([]byte(s.cfg.Auth.JWTSecret), payload, signature) {
		return httpx.ErrForbidden("This link is not valid.")
	}
	return nil
}

// signedPayload builds the string a link's signature covers. Every field that
// changes what the link returns has to be in here, or it can be edited freely.
func signedPayload(docPublicID, action string, version int, viewerID, expires int64) string {
	return fmt.Sprintf("%s|%s|%d|%d|%d", docPublicID, action, version, viewerID, expires)
}

// AsOfVersion returns a view of the document as it stood at one revision.
//
// Older revisions keep their own path, nonce and key — replacing a file does
// not re-encrypt what came before — so reading one is the same operation as
// reading the current document, over different bytes.
func (s *Service) AsOfVersion(ctx context.Context, doc *Document, version int) (*Document, error) {
	if version <= 0 || version == doc.Version {
		return doc, nil
	}

	v, err := s.repo.VersionAt(ctx, doc.TenantID, doc.ID, version)
	if err != nil {
		return nil, httpx.ErrNotFound("That version")
	}

	at := *doc
	at.StoredPath = v.StoredPath
	at.Nonce = v.Nonce
	at.EncryptionKeyID = v.EncryptionKeyID
	at.OriginalName = v.OriginalName
	at.MimeType = v.MimeType
	at.SizeBytes = v.SizeBytes
	at.Version = v.Version
	return &at, nil
}

// LogAccess records a read against the document, attributing it to the caller.
func (s *Service) LogAccess(ctx context.Context, tenantID int64, doc *Document, action string) {
	var userID *int64
	if actor := appctx.ActorFrom(ctx); actor != nil {
		id := actor.UserID
		userID = &id
	}
	s.repo.LogAccess(ctx, tenantID, doc.ID, userID, action,
		appctx.ClientIP(ctx), appctx.UserAgent(ctx))
}
