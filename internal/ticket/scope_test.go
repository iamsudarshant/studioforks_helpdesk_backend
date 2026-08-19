package ticket

import (
	"strings"
	"testing"

	"github.com/karmamgmt/complydesk/internal/appctx"
)

// Scope.apply is the second half of tenant isolation: the tenant clause keeps
// one client out of another's data, and this keeps one *user* out of data they
// have no business reading inside their own client. A missing clause here would
// widen a read silently, so the SQL fragments are asserted directly.
func TestScopeApply(t *testing.T) {
	id := int64(7)

	t.Run("an unrestricted scope adds no clause", func(t *testing.T) {
		var where []string
		var args []any
		Scope{}.Apply(&where, &args)

		if len(where) != 0 || len(args) != 0 {
			t.Fatalf("expected no clause, got where=%v args=%v", where, args)
		}
	})

	t.Run("a requester scope restricts to one person", func(t *testing.T) {
		var where []string
		var args []any
		Scope{RequesterID: &id}.Apply(&where, &args)

		if len(where) != 1 || !strings.Contains(where[0], "t.requester_id = ?") {
			t.Fatalf("unexpected clause: %v", where)
		}
		if len(args) != 1 || args[0] != id {
			t.Fatalf("unexpected args: %v", args)
		}
	})

	t.Run("an empty non-nil scope matches nothing, never everything", func(t *testing.T) {
		// This is the dangerous case. A user scoped to zero entities must see no
		// tickets; treating the empty slice as "unrestricted" would show them
		// every ticket in the client.
		var where []string
		var args []any
		Scope{EntityIDs: []int64{}}.Apply(&where, &args)

		if len(where) != 1 || where[0] != "1 = 0" {
			t.Fatalf("expected an impossible clause, got %v", where)
		}
		if len(args) != 0 {
			t.Fatalf("expected no args, got %v", args)
		}
	})

	t.Run("a nil dimension is unrestricted while a sibling is restricted", func(t *testing.T) {
		var where []string
		var args []any
		Scope{EntityIDs: []int64{1, 2}, SiteIDs: nil}.Apply(&where, &args)

		joined := strings.Join(where, " AND ")
		if !strings.Contains(joined, "t.entity_id IN (?,?)") {
			t.Fatalf("entity clause missing: %v", where)
		}
		if strings.Contains(joined, "t.site_id") {
			t.Fatalf("a nil dimension must add no clause: %v", where)
		}
		if len(args) != 2 {
			t.Fatalf("expected 2 args, got %v", args)
		}
	})

	t.Run("every dimension is applied together", func(t *testing.T) {
		var where []string
		var args []any
		Scope{
			RequesterID:   &id,
			EntityIDs:     []int64{1},
			SiteIDs:       []int64{2},
			DepartmentIDs: []int64{3},
			CategoryIDs:   []int64{4},
		}.Apply(&where, &args)

		for _, col := range []string{"t.requester_id", "t.entity_id", "t.site_id", "t.department_id", "t.category_id"} {
			if !strings.Contains(strings.Join(where, " AND "), col) {
				t.Errorf("clause for %s missing: %v", col, where)
			}
		}
		if len(args) != 5 {
			t.Errorf("expected 5 args, got %d: %v", len(args), args)
		}
	})
}

// ScopeFor translates permissions into a scope. It must never fall open.
func TestScopeFor(t *testing.T) {
	svc := &Service{}

	t.Run("a nil actor sees nothing", func(t *testing.T) {
		got := svc.ScopeFor(nil)
		if got.RequesterID == nil || *got.RequesterID != -1 {
			t.Fatalf("expected an unmatchable requester, got %+v", got)
		}
	})

	t.Run("ticket.view.all is unrestricted", func(t *testing.T) {
		actor := &appctx.Actor{Permissions: map[string]struct{}{"ticket.view.all": {}}}
		got := svc.ScopeFor(actor)
		if got.RequesterID != nil || got.EntityIDs != nil {
			t.Fatalf("expected an unrestricted scope, got %+v", got)
		}
	})

	t.Run("no ticket permission at all falls back to own tickets", func(t *testing.T) {
		actor := &appctx.Actor{UserID: 42, Permissions: map[string]struct{}{}}
		got := svc.ScopeFor(actor)
		if got.RequesterID == nil || *got.RequesterID != 42 {
			t.Fatalf("expected own-tickets scope, got %+v", got)
		}
	})

	t.Run("a scoped viewer with no allocation sees nothing, not everything", func(t *testing.T) {
		// The dangerous case: all four dimensions nil would otherwise read as
		// "unrestricted on all four" and hand a Client Executive the whole
		// client. Segmentation is the definition of that role, so its absence
		// must fail closed.
		actor := &appctx.Actor{UserID: 9, Permissions: map[string]struct{}{"ticket.view.scope": {}}}
		got := svc.ScopeFor(actor)
		if got.RequesterID == nil || *got.RequesterID != -1 {
			t.Fatalf("expected an unmatchable scope, got %+v", got)
		}
	})

	t.Run("an explicitly empty dimension also matches nothing", func(t *testing.T) {
		actor := &appctx.Actor{
			UserID:      9,
			Permissions: map[string]struct{}{"ticket.view.scope": {}},
			Scopes:      appctx.Scopes{Entities: []int64{}},
		}
		var where []string
		var args []any
		svc.ScopeFor(actor).Apply(&where, &args)
		if len(where) != 1 || where[0] != "1 = 0" {
			t.Fatalf("expected an impossible clause, got %v", where)
		}
	})

	t.Run("ticket.view.scope carries the actor's dimensions", func(t *testing.T) {
		actor := &appctx.Actor{
			UserID:      9,
			Permissions: map[string]struct{}{"ticket.view.scope": {}},
			Scopes:      appctx.Scopes{Entities: []int64{5}, Sites: []int64{6}},
		}
		got := svc.ScopeFor(actor)
		if got.RequesterID != nil {
			t.Fatalf("a scoped viewer is not limited to their own tickets: %+v", got)
		}
		if len(got.EntityIDs) != 1 || got.EntityIDs[0] != 5 {
			t.Fatalf("entities not carried: %+v", got)
		}
	})
}
