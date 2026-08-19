package app

import (
	"context"

	"github.com/karmamgmt/complydesk/internal/appctx"
	"github.com/karmamgmt/complydesk/internal/auth"
	"github.com/karmamgmt/complydesk/internal/org"
	"github.com/karmamgmt/complydesk/internal/platform"
	"github.com/karmamgmt/complydesk/internal/tenant"
)

// lookup resolves internal ids into the display references the /auth/me payload
// carries. It lives in the composition root so auth does not depend on org.
type lookup struct {
	org     *org.Repository
	tenants *tenant.Repository
	db      *platform.DB
}

func newLookup(o *org.Repository, t *tenant.Repository, db *platform.DB) auth.ProfileLookup {
	return &lookup{org: o, tenants: t, db: db}
}

func (l *lookup) EntityRefs(ctx context.Context, tenantID int64, ids []int64) []auth.Ref {
	if len(ids) == 0 {
		return []auth.Ref{}
	}
	rows, err := l.org.Entities(ctx, appctx.OneClient(tenantID), false, ids, platform.Page{}, org.OrgFilter{})
	if err != nil {
		return []auth.Ref{}
	}
	out := make([]auth.Ref, 0, len(rows))
	for _, e := range rows {
		out = append(out, auth.Ref{ID: e.PublicID, Code: e.Code, Name: e.Name})
	}
	return out
}

func (l *lookup) SiteRefs(ctx context.Context, tenantID int64, ids []int64) []auth.Ref {
	if len(ids) == 0 {
		return []auth.Ref{}
	}
	rows, err := l.org.Sites(ctx, appctx.OneClient(tenantID), nil, false, ids, platform.Page{}, org.OrgFilter{})
	if err != nil {
		return []auth.Ref{}
	}
	out := make([]auth.Ref, 0, len(rows))
	for _, s := range rows {
		out = append(out, auth.Ref{ID: s.PublicID, Code: s.Code, Name: s.Name})
	}
	return out
}

func (l *lookup) DepartmentRefs(ctx context.Context, tenantID int64, ids []int64) []auth.Ref {
	if len(ids) == 0 {
		return []auth.Ref{}
	}
	rows, err := l.org.Departments(ctx, appctx.OneClient(tenantID), false, ids, platform.Page{}, org.OrgFilter{})
	if err != nil {
		return []auth.Ref{}
	}
	out := make([]auth.Ref, 0, len(rows))
	for _, d := range rows {
		out = append(out, auth.Ref{ID: d.PublicID, Code: d.Code, Name: d.Name})
	}
	return out
}

func (l *lookup) CategoryRefs(ctx context.Context, tenantID int64, ids []int64) []auth.Ref {
	if len(ids) == 0 {
		return []auth.Ref{}
	}

	type row struct {
		PublicID string `db:"public_id"`
		Key      string `db:"category_key"`
		Name     string `db:"name"`
	}
	rows := []row{}

	args := append([]any{tenantID}, platform.Int64Args(ids)...)
	q := `SELECT public_id, category_key, name FROM categories
	      WHERE tenant_id = ? AND id IN (` + platform.Placeholders(len(ids)) + `) AND deleted_at IS NULL
	      ORDER BY sort_order, name`

	if err := l.db.Primary.SelectContext(ctx, &rows, q, args...); err != nil {
		return []auth.Ref{}
	}

	out := make([]auth.Ref, 0, len(rows))
	for _, c := range rows {
		out = append(out, auth.Ref{ID: c.PublicID, Code: c.Key, Name: c.Name})
	}
	return out
}

func (l *lookup) Features(ctx context.Context, tenantID int64) map[string]bool {
	features, err := l.tenants.Features(ctx, tenantID)
	if err != nil {
		return map[string]bool{}
	}
	return features
}
