package document

import "testing"

// A file's extension is a claim by whoever uploaded it. These cases pin the
// rule that the bytes have to back the claim up, because the failure mode is
// quiet: a rejected upload is visible, but an executable accepted and stored as
// "application/pdf" looks exactly like a working feature until someone opens it.
func TestMIMEMatchesRejectsRenames(t *testing.T) {
	pe := append([]byte{'M', 'Z', 0x90, 0x00, 0x03}, make([]byte, 600)...)
	ole := []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1, 0x00}

	cases := []struct {
		name     string
		ext      string
		detected string
		head     []byte
		want     bool
	}{
		{"a real PDF", "pdf", "application/pdf", []byte("%PDF-1.7\n%..."), true},
		{"an executable called .pdf", "pdf", "application/octet-stream", pe, false},
		{"an executable called .docx", "docx", "application/octet-stream", pe, false},
		{"an executable called .zip", "zip", "application/octet-stream", pe, false},
		{"an executable called .xls", "xls", "application/octet-stream", pe, false},
		{"a real docx", "docx", "application/zip", []byte{'P', 'K', 3, 4, 0x14}, true},
		{"a real xls", "xls", "application/x-ole-storage", ole, true},
		{"an empty archive", "zip", "application/zip", []byte{'P', 'K', 5, 6}, true},
		{"a text file", "txt", "text/plain", []byte("hello\n"), true},
		{"HTML called .txt", "txt", "text/html", []byte("<html><script>"), false},
		{"an extension we do not accept", "exe", "application/octet-stream", pe, false},

		// A PDF need not declare itself at byte zero. Requiring that rejected
		// files that open perfectly well — the header is allowed anywhere in
		// the first 1024 bytes, and byte-order marks, leading whitespace and
		// bytes left by a signing step all push it along.
		{"a PDF behind a byte-order mark", "pdf", "application/octet-stream",
			append([]byte{0xEF, 0xBB, 0xBF}, []byte("%PDF-1.7\n")...), true},
		{"a PDF behind leading whitespace", "pdf", "text/plain",
			[]byte("\n  \n%PDF-1.4\ntrailer"), true},
		{"a PDF declared late in the window", "pdf", "application/octet-stream",
			append(make([]byte, 900), []byte("%PDF-1.6")...), true},
		// Still a rename: the marker sits past the window a reader looks in.
		{"a marker beyond the search window", "pdf", "application/octet-stream",
			append(make([]byte, 1100), []byte("%PDF-1.6")...), false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mimeMatches(tc.ext, tc.detected, tc.head); got != tc.want {
				t.Fatalf("mimeMatches(%q, %q) = %v, want %v", tc.ext, tc.detected, got, tc.want)
			}
		})
	}
}

// Inline rendering is the one place a stored file is handed to the browser as
// something it will interpret, so the list of types allowed to do that is worth
// asserting rather than assuming.
func TestOnlySafeTypesRenderInline(t *testing.T) {
	for _, mimeType := range []string{"application/pdf", "image/png", "text/plain"} {
		if !CanPreviewInline(mimeType) {
			t.Errorf("%s should be previewable inline", mimeType)
		}
	}
	// An SVG is a script container and HTML executes against this origin.
	for _, mimeType := range []string{
		"image/svg+xml", "text/html", "application/zip",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	} {
		if CanPreviewInline(mimeType) {
			t.Errorf("%s must not render inline", mimeType)
		}
	}
}
