package tenant

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// tenantCounts is the per-client usage snapshot merged into the roster list.
type tenantCounts struct {
	EntityCount   int64
	UserCount     int64
	TicketCount   int64
	StorageUsedMB int64
}

type tenantUsageRow struct {
	TenantID     int64 `db:"tenant_id"`
	EntityCount  int64 `db:"entity_count"`
	UserCount    int64 `db:"user_count"`
	TicketCount  int64 `db:"ticket_count"`
	StorageBytes int64 `db:"storage_bytes"`
}

// ListUsageCounts returns, keyed by tenant id, the entity/user/ticket counts and
// the storage used. One batched query covers the whole page so the roster does
// not fan out a SELECT per row. A tenant that disappears mid-query simply has no
// entry in the map.
func (r *Repository) ListUsageCounts(ctx context.Context, tenantIDs []int64) (map[int64]tenantCounts, error) {
	if len(tenantIDs) == 0 {
		return map[int64]tenantCounts{}, nil
	}

	query, args, err := sqlx.In(`
		SELECT t.id AS tenant_id,
		       (SELECT COUNT(*) FROM entities e WHERE e.tenant_id = t.id AND e.deleted_at IS NULL) AS entity_count,
		       (SELECT COUNT(*) FROM users u WHERE u.tenant_id = t.id AND u.deleted_at IS NULL) AS user_count,
		       (SELECT COUNT(*) FROM tickets tk WHERE tk.tenant_id = t.id AND tk.deleted_at IS NULL) AS ticket_count,
		       COALESCE((SELECT SUM(d.size_bytes) FROM documents d WHERE d.tenant_id = t.id AND d.deleted_at IS NULL), 0) AS storage_bytes
		  FROM tenants t
		 WHERE t.id IN (?) AND t.deleted_at IS NULL`, tenantIDs)
	if err != nil {
		return nil, fmt.Errorf("building usage-count query: %w", err)
	}

	var rows []tenantUsageRow
	if err := r.db.Primary.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("loading usage counts: %w", err)
	}

	out := make(map[int64]tenantCounts, len(rows))
	for _, row := range rows {
		out[row.TenantID] = tenantCounts{
			EntityCount:   row.EntityCount,
			UserCount:     row.UserCount,
			TicketCount:   row.TicketCount,
			StorageUsedMB: row.StorageBytes / (1024 * 1024),
		}
	}
	return out, nil
}

// UsageSummary is the single-client usage report behind GET /admin/tenants/{id}/usage.
type UsageSummary struct {
	TicketsByMonth []monthlyCount `json:"tickets_by_month"`
	Users          userUsage      `json:"users"`
	Storage        storageUsage   `json:"storage"`
	APICallsByDay  []dailyCount   `json:"api_calls_by_day"`
}

type monthlyCount struct {
	Month string `json:"month"`
	Count int64  `json:"count"`
}

type userUsage struct {
	Total  int64 `json:"total"`
	Active int64 `json:"active"`
}

type storageUsage struct {
	UsedMB  int64 `json:"used_mb"`
	LimitMB int64 `json:"limit_mb"`
}

type dailyCount struct {
	Date  string `json:"date"`
	Count int64  `json:"count"`
}

// storageLimitMB is the published storage ceiling for a workspace. It is not a
// per-tenant setting; the constant keeps the report truthful until quotas exist.
const storageLimitMB = 10240

// Usage composes the usage report for one client.
func (r *Repository) Usage(ctx context.Context, tenantID int64) (*UsageSummary, error) {
	summary := &UsageSummary{
		TicketsByMonth: lastNMonths(12),
		APICallsByDay:  lastNDays(30),
	}

	firstMonth := summary.TicketsByMonth[0].Month
	monthRows, err := r.ticketCountsByMonth(ctx, tenantID, firstMonth)
	if err != nil {
		return nil, err
	}
	for i := range summary.TicketsByMonth {
		if c, ok := monthRows[summary.TicketsByMonth[i].Month]; ok {
			summary.TicketsByMonth[i].Count = c
		}
	}

	firstDay := summary.APICallsByDay[0].Date
	dayRows, err := r.activityByDay(ctx, tenantID, firstDay)
	if err != nil {
		return nil, err
	}
	for i := range summary.APICallsByDay {
		if c, ok := dayRows[summary.APICallsByDay[i].Date]; ok {
			summary.APICallsByDay[i].Count = c
		}
	}

	users, storage, err := r.userAndStorageUsage(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	summary.Users = users
	summary.Storage = storage

	return summary, nil
}

func (r *Repository) ticketCountsByMonth(ctx context.Context, tenantID int64, fromMonth string) (map[string]int64, error) {
	var rows []struct {
		Month string `db:"month"`
		Count int64  `db:"count"`
	}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT DATE_FORMAT(created_at, '%Y-%m') AS month, COUNT(*) AS count
		  FROM tickets
		 WHERE tenant_id = ? AND deleted_at IS NULL
		   AND DATE_FORMAT(created_at, '%Y-%m') >= ?
		 GROUP BY month`, tenantID, fromMonth)
	if err != nil {
		return nil, fmt.Errorf("loading ticket counts by month: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Month] = row.Count
	}
	return out, nil
}

func (r *Repository) activityByDay(ctx context.Context, tenantID int64, fromDate string) (map[string]int64, error) {
	var rows []struct {
		Day   string `db:"day"`
		Count int64  `db:"count"`
	}
	err := r.db.Primary.SelectContext(ctx, &rows, `
		SELECT DATE_FORMAT(created_at, '%Y-%m-%d') AS day, COUNT(*) AS count
		  FROM audit_logs
		 WHERE tenant_id = ? AND created_at >= ?
		 GROUP BY day`, tenantID, fromDate)
	if err != nil {
		return nil, fmt.Errorf("loading activity by day: %w", err)
	}
	out := make(map[string]int64, len(rows))
	for _, row := range rows {
		out[row.Day] = row.Count
	}
	return out, nil
}

func (r *Repository) userAndStorageUsage(ctx context.Context, tenantID int64) (userUsage, storageUsage, error) {
	var users userUsage
	if err := r.db.Primary.GetContext(ctx, &users, `
		SELECT COUNT(*) AS total,
		       COALESCE(SUM(CASE WHEN status = 'ACTIVE' THEN 1 ELSE 0 END), 0) AS active
		  FROM users
		 WHERE tenant_id = ? AND deleted_at IS NULL`, tenantID); err != nil {
		return users, storageUsage{}, fmt.Errorf("loading user usage: %w", err)
	}

	var usedBytes int64
	if err := r.db.Primary.GetContext(ctx, &usedBytes, `
		SELECT COALESCE(SUM(size_bytes), 0)
		  FROM documents
		 WHERE tenant_id = ? AND deleted_at IS NULL`, tenantID); err != nil {
		return users, storageUsage{}, fmt.Errorf("loading storage usage: %w", err)
	}

	return users, storageUsage{UsedMB: usedBytes / (1024 * 1024), LimitMB: storageLimitMB}, nil
}

func lastNMonths(n int) []monthlyCount {
	out := make([]monthlyCount, 0, n)
	now := time.Now()
	for i := n - 1; i >= 0; i-- {
		t := now.AddDate(0, -i, 0)
		out = append(out, monthlyCount{Month: t.Format("2006-01")})
	}
	return out
}

func lastNDays(n int) []dailyCount {
	out := make([]dailyCount, 0, n)
	now := time.Now()
	for i := n - 1; i >= 0; i-- {
		t := now.AddDate(0, 0, -i)
		out = append(out, dailyCount{Date: t.Format("2006-01-02")})
	}
	return out
}
