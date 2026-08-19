package tenant

import (
	"fmt"
	"net/http"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"

	"github.com/karmamgmt/complydesk/internal/httpx"
)

// A client with no uploaded logo used to fall back to the ComplyDesk mark,
// which made every client portal look identical and defeated the dual-branding
// rule — an employee of Ampersand should see Ampersand on their own login page.
//
// This generates a monogram from the client's own name and brand colour and
// serves it as a real image. Nothing is stored: the logo is derived from the
// tenant row on request, so renaming a client renames its logo, and there is no
// file to migrate, back up or leave orphaned. The moment real artwork is
// uploaded, `logo_path` takes precedence and this is never reached.

// MonogramSVG builds a wordmark: the client's initials in a rounded tile,
// followed by the name.
func MonogramSVG(name, code, colour string) string {
	initials := Initials(name, code)
	if strings.TrimSpace(colour) == "" {
		colour = "#1B5E9E"
	}

	// The label is truncated rather than shrunk: a long legal name at a smaller
	// size becomes unreadable at the 26px the top bar renders it at.
	label := []rune(name)
	if len(label) > 26 {
		label = append(label[:25], '…')
	}

	// Two initials need a smaller size than one to sit inside the same tile.
	fontSize := 15
	if len([]rune(initials)) > 1 {
		fontSize = 13
	}

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 226 40" role="img" aria-label="%s">
  <title>%s</title>
  <g fill="none" fill-rule="evenodd">
    <rect width="32" height="32" x="2" y="4" rx="9" fill="%s"/>
    <text x="18" y="25" text-anchor="middle"
      font-family="Inter, system-ui, -apple-system, 'Segoe UI', sans-serif"
      font-size="%d" font-weight="700" letter-spacing="-0.2" fill="#FFFFFF">%s</text>
    <text x="44" y="25"
      font-family="Inter, system-ui, -apple-system, 'Segoe UI', sans-serif"
      font-size="17" font-weight="600" letter-spacing="-0.3" fill="#111417">%s</text>
  </g>
</svg>`, escapeXML(name), escapeXML(name), escapeXML(colour), fontSize,
		escapeXML(initials), escapeXML(string(label)))
}

// Initials takes the first letter of the first two significant words, so
// "Ampersand Group Holdings Pvt Ltd" becomes "AG" rather than "AGHPL".
//
// Corporate suffixes are skipped: without that, most Indian clients would
// collide on the same two letters.
func Initials(name, code string) string {
	skip := map[string]bool{
		"pvt": true, "private": true, "ltd": true, "limited": true, "llp": true,
		"inc": true, "corp": true, "corporation": true, "co": true,
		"the": true, "and": true, "of": true,
	}

	var letters []rune
	for _, word := range strings.Fields(name) {
		clean := strings.ToLower(strings.TrimFunc(word, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		}))
		if clean == "" || skip[clean] {
			continue
		}
		letters = append(letters, unicode.ToUpper([]rune(clean)[0]))
		if len(letters) == 2 {
			break
		}
	}

	if len(letters) > 0 {
		return string(letters)
	}

	// Fall back to the client code, then to a neutral mark, so this can never
	// return an empty tile.
	if code != "" {
		runes := []rune(strings.ToUpper(code))
		if len(runes) > 2 {
			runes = runes[:2]
		}
		return string(runes)
	}
	return "?"
}

func escapeXML(s string) string {
	return strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;",
	).Replace(s)
}

// MonogramURL is where a client's generated logo lives.
func MonogramURL(slug string) string {
	return "/api/v1/public/branding/" + slug + "/logo.svg"
}

// PublicBrandingRoutes mounts the unauthenticated branding surface.
func (h *Handler) PublicBrandingRoutes(r chi.Router) {
	r.Get("/public/branding/{slug}/logo.svg", h.serveMonogram)
}

// serveMonogram renders a client's generated logo.
//
// Public and unauthenticated, like the rest of `/public`: the login screen has
// to paint branding before anyone has signed in. It discloses only the client's
// name and colour, which the login page shows anyway.
func (h *Handler) serveMonogram(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")

	row, err := h.svc.repo.BySlug(r.Context(), slug)
	if err != nil {
		httpx.Fail(w, r, httpx.ErrNotFound("That workspace"))
		return
	}

	branding, err := h.svc.repo.Branding(r.Context(), row.ID)
	colour := "#1B5E9E"
	if err == nil && branding.PrimaryColor != "" {
		colour = branding.PrimaryColor
	}

	svg := MonogramSVG(row.Name, row.ClientCode.String, colour)

	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Derived from the tenant row, so it changes only when the client is
	// renamed or rebranded. An hour is long enough to be cheap and short enough
	// that a rebrand appears the same working day.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(svg))
}
