package platform

import (
	"context"
	"fmt"
	"strings"

	"github.com/jmoiron/sqlx"

	"github.com/karmamgmt/complydesk/internal/appctx"
)

// ResolveClientRef turns a client reference — public id, slug or client code —
// into an internal tenant id, refusing anything outside the caller's reach.
//
// It lives here rather than in one feature package because four of them now ask
// the same question. A staff user with no client selected reaches every client
// they are assigned to, so "which client is this write for?" cannot be answered
// from the tenant header alone: the request has to name one, and the name has to
// be checked against what the caller may actually see. Returning
// ErrSentinelNotFound for an out-of-reach client — rather than a permission
// error — keeps the existence of other clients undisclosed.
func ResolveClientRef(ctx context.Context, db *sqlx.DB, reach appctx.ClientReach, ref string) (int64, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, ErrSentinelNotFound
	}

	where := []string{"deleted_at IS NULL", "(public_id = ? OR slug = ? OR client_code = ?)"}
	args := []any{ref, strings.ToLower(ref), strings.ToUpper(ref)}

	switch {
	case reach.All:
	case len(reach.TenantIDs) > 0:
		where = append(where, "id IN ("+Placeholders(len(reach.TenantIDs))+")")
		args = append(args, Int64Args(reach.TenantIDs)...)
	default:
		// No reach at all: match nothing rather than everything.
		where = append(where, "1 = 0")
	}

	var id int64
	if err := db.GetContext(ctx, &id,
		`SELECT id FROM tenants WHERE `+strings.Join(where, " AND ")+` LIMIT 1`, args...); err != nil {
		if IsNotFound(err) {
			return 0, ErrSentinelNotFound
		}
		return 0, fmt.Errorf("resolving client: %w", err)
	}
	return id, nil
}
