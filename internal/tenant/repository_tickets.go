package tenant

import (
	"context"
	"fmt"
)

// HasTickets reports whether a client has ever had a ticket raised against it.
//
// The question behind the client-code lock. Deliberately counts *any* ticket,
// including soft-deleted ones: the number was issued, may have been quoted in an
// email or printed on a letter, and deleting the row does not un-issue it.
//
// EXISTS rather than COUNT, so it stops at the first row.
func (r *Repository) HasTickets(ctx context.Context, tenantID int64) (bool, error) {
	var found bool
	err := r.db.Primary.GetContext(ctx, &found,
		`SELECT EXISTS (SELECT 1 FROM tickets WHERE tenant_id = ?)`, tenantID)
	if err != nil {
		return false, fmt.Errorf("checking whether the client has tickets: %w", err)
	}
	return found, nil
}
