package analytics

import (
	"net/http/httptest"
	"testing"
	"time"
)

// The dashboard filter is the one piece of this package with no database in the
// way, and it is where the bug was: the summary ignored the range entirely, so
// every preset returned the same numbers. These pin the parsing down.

func windowFor(t *testing.T, query string) *Window {
	t.Helper()
	return window(httptest.NewRequest("GET", "/dashboard/summary?"+query, nil))
}

func TestWindowPresets(t *testing.T) {
	now := time.Now().UTC()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	cases := []struct {
		query string
		from  time.Time
		open  bool // no lower bound at all
	}{
		{query: "range=today", from: midnight},
		{query: "range=last7", from: midnight.AddDate(0, 0, -6)},
		{query: "range=last30", from: midnight.AddDate(0, 0, -29)},
		{query: "range=last90", from: midnight.AddDate(0, 0, -89)},
		{query: "range=mtd", from: time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)},
		{query: "range=ytd", from: time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)},
		{query: "range=all", open: true},
		{query: "", open: true},
		// An unparseable value must not fail the page — a dashboard is a
		// read-only overview and the unfiltered view is the safe fallback.
		{query: "range=nonsense", open: true},
	}

	for _, tc := range cases {
		w := windowFor(t, tc.query)
		if tc.open {
			if w.From != nil || w.To != nil {
				t.Errorf("%q: expected no bounds, got from=%v to=%v", tc.query, w.From, w.To)
			}
			continue
		}
		if w.From == nil {
			t.Fatalf("%q: expected a lower bound", tc.query)
		}
		if !w.From.Equal(tc.from) {
			t.Errorf("%q: from = %v, want %v", tc.query, w.From, tc.from)
		}
		if w.To != nil {
			t.Errorf("%q: a preset should leave the upper end open, got %v", tc.query, w.To)
		}
	}
}

// Monday, because that is where the Indian payroll and compliance week starts.
func TestWindowWeekToDateStartsMonday(t *testing.T) {
	w := windowFor(t, "range=wtd")
	if w.From == nil {
		t.Fatal("expected a lower bound")
	}
	if got := w.From.Weekday(); got != time.Monday {
		t.Errorf("week-to-date starts on %v, want Monday", got)
	}
	if w.From.After(time.Now().UTC()) {
		t.Error("week-to-date starts in the future")
	}
}

// `to` names a whole day. Stopping at midnight would omit everything raised
// since, which is most of what someone asking for "up to today" wants to see.
func TestWindowExplicitDatesAreDayInclusive(t *testing.T) {
	w := windowFor(t, "from=2026-08-01&to=2026-08-18")

	wantFrom := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if w.From == nil || !w.From.Equal(wantFrom) {
		t.Errorf("from = %v, want %v", w.From, wantFrom)
	}

	// Exclusive upper bound on the following midnight is what makes the named
	// day inclusive.
	wantTo := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	if w.To == nil || !w.To.Equal(wantTo) {
		t.Errorf("to = %v, want %v (the day after, exclusive)", w.To, wantTo)
	}
}

// Either end alone is a real question: "since the first of the month",
// "everything up to the audit date".
func TestWindowAcceptsOneEndedRanges(t *testing.T) {
	if w := windowFor(t, "from=2026-08-01"); w.From == nil || w.To != nil {
		t.Errorf("from-only: got from=%v to=%v", w.From, w.To)
	}
	if w := windowFor(t, "to=2026-08-01"); w.From != nil || w.To == nil {
		t.Errorf("to-only: got from=%v to=%v", w.From, w.To)
	}
}

// A stale preset left in the URL must not second-guess dates the user typed.
func TestWindowExplicitDatesBeatThePreset(t *testing.T) {
	w := windowFor(t, "range=today&from=2026-01-01&to=2026-01-31")

	wantFrom := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if w.From == nil || !w.From.Equal(wantFrom) {
		t.Errorf("from = %v, want the explicit %v rather than the preset", w.From, wantFrom)
	}
}

// A malformed date is ignored rather than rejected, for the same reason an
// unknown preset is.
func TestWindowIgnoresMalformedDates(t *testing.T) {
	w := windowFor(t, "from=not-a-date&to=13/45/2026")
	if w.From != nil || w.To != nil {
		t.Errorf("expected the malformed range to be ignored, got from=%v to=%v", w.From, w.To)
	}
}

// Apply is what folds the window into a WHERE builder; a nil receiver has to be
// a no-op, because that is how every caller without a range stays unchanged.
func TestWindowApply(t *testing.T) {
	var where []string
	var args []any

	var absent *Window
	absent.Apply(&where, &args)
	if len(where) != 0 || len(args) != 0 {
		t.Errorf("nil window added %v / %v", where, args)
	}

	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	(&Window{From: &from, To: &to}).Apply(&where, &args)

	if len(where) != 2 || len(args) != 2 {
		t.Fatalf("expected two clauses and two args, got %v / %v", where, args)
	}
	if where[0] != "t.created_at >= ?" || where[1] != "t.created_at < ?" {
		t.Errorf("unexpected clauses: %v", where)
	}
}
