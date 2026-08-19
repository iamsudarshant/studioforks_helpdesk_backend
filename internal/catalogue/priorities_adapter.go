package catalogue

import "context"

// KnownPriority answers the ticket engine's question: is this a level the
// client offers, and what is its canonical key.
//
// A thin adapter over PriorityByKey so the ticket package can depend on a
// two-method interface of plain strings rather than on this package's types.
func (r *Repository) KnownPriority(ctx context.Context, tenantID int64, key string) (string, error) {
	p, err := r.PriorityByKey(ctx, tenantID, key)
	if err != nil || p == nil {
		return "", err
	}
	return p.Key, nil
}
