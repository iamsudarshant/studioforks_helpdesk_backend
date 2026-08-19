package platform

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultPerPage = 25
	MaxPerPage     = 200
)

// Page describes a validated page/per_page/sort request.
type Page struct {
	Page    int
	PerPage int
	SortBy  string
	SortDir string // ASC | DESC
	Cursor  string
}

func (p Page) Offset() int { return (p.Page - 1) * p.PerPage }

// Meta is the pagination block returned in the response envelope.
type Meta struct {
	Page       int    `json:"page"`
	PerPage    int    `json:"per_page"`
	Total      int64  `json:"total"`
	TotalPages int    `json:"total_pages"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func NewMeta(p Page, total int64) *Meta {
	pages := 0
	if p.PerPage > 0 {
		pages = int((total + int64(p.PerPage) - 1) / int64(p.PerPage))
	}
	return &Meta{Page: p.Page, PerPage: p.PerPage, Total: total, TotalPages: pages}
}

// ParsePage reads page/per_page/sort/cursor from the query string. sortable
// whitelists the columns a caller may sort by; anything else falls back to
// fallbackSort, which keeps user input out of the ORDER BY clause entirely.
func ParsePage(r *http.Request, sortable map[string]string, fallbackSort string) Page {
	q := r.URL.Query()

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}

	perPage, _ := strconv.Atoi(q.Get("per_page"))
	switch {
	case perPage < 1:
		perPage = DefaultPerPage
	case perPage > MaxPerPage:
		perPage = MaxPerPage
	}

	sortBy, sortDir := fallbackSort, "DESC"
	if raw := strings.TrimSpace(q.Get("sort")); raw != "" {
		dir := "ASC"
		if strings.HasPrefix(raw, "-") {
			dir = "DESC"
			raw = raw[1:]
		}
		if col, ok := sortable[raw]; ok {
			sortBy, sortDir = col, dir
		}
	}

	return Page{
		Page:    page,
		PerPage: perPage,
		SortBy:  sortBy,
		SortDir: sortDir,
		Cursor:  strings.TrimSpace(q.Get("cursor")),
	}
}

// OrderBy renders a safe ORDER BY fragment. Both parts come from the whitelist
// in ParsePage, never from raw input.
func (p Page) OrderBy() string {
	if p.SortBy == "" {
		return ""
	}
	return fmt.Sprintf(" ORDER BY %s %s", p.SortBy, p.SortDir)
}

// QueryDates parses an inclusive from/to date-range pair from the query string.
// Bare dates are widened to cover the whole day in UTC.
func QueryDates(r *http.Request, fromKey, toKey string) (from, to *time.Time) {
	q := r.URL.Query()
	if v := strings.TrimSpace(q.Get(fromKey)); v != "" {
		if t, ok := parseFlexibleTime(v); ok {
			from = &t
		}
	}
	if v := strings.TrimSpace(q.Get(toKey)); v != "" {
		if t, ok := parseFlexibleTime(v); ok {
			if len(v) == 10 { // bare date -> end of day
				t = t.Add(24*time.Hour - time.Nanosecond)
			}
			to = &t
		}
	}
	return from, to
}

func parseFlexibleTime(v string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// QueryStrings returns all values for a repeated query key, trimmed and
// de-duplicated, preserving order.
func QueryStrings(r *http.Request, key string) []string {
	raw := r.URL.Query()[key]
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		// Tolerate both ?k=a&k=b and ?k=a,b
		for _, part := range strings.Split(v, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if _, dup := seen[part]; dup {
				continue
			}
			seen[part] = struct{}{}
			out = append(out, part)
		}
	}
	return out
}

// QueryBool reads an optional boolean flag. Missing or unparsable values
// return nil so callers can distinguish "absent" from "false".
func QueryBool(r *http.Request, key string) *bool {
	v := strings.TrimSpace(r.URL.Query().Get(key))
	if v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil
	}
	return &b
}

// Placeholders renders "?, ?, ?" for an IN clause of n elements.
func Placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// Int64Args converts an id slice to the []any that database/sql expects.
func Int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// StringArgs converts a string slice to []any for an IN clause.
func StringArgs(vals []string) []any {
	out := make([]any, len(vals))
	for i, v := range vals {
		out[i] = v
	}
	return out
}
