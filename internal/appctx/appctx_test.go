package appctx

import "testing"

// MayAccessTenant is the single choke point for tenant isolation: every
// tenant-scoped request passes through it, and a false negative here would let
// one client read another's data. These cases pin each branch.
func TestMayAccessTenant(t *testing.T) {
	const (
		karma   = int64(1)
		clientA = int64(10)
		clientB = int64(20)
		clientC = int64(30)
	)

	cases := []struct {
		name   string
		actor  *Actor
		tenant int64
		want   bool
	}{
		{
			name:   "nil actor is refused",
			actor:  nil,
			tenant: clientA,
		},
		{
			name:   "a zero tenant is refused rather than treated as a wildcard",
			actor:  &Actor{IsSuperAdmin: true},
			tenant: 0,
		},
		{
			name:   "a super admin reaches any client",
			actor:  &Actor{IsSuperAdmin: true, IsStaff: true, TenantID: karma},
			tenant: clientC,
			want:   true,
		},
		{
			name:   "a client user reaches their own workspace",
			actor:  &Actor{TenantID: clientA},
			tenant: clientA,
			want:   true,
		},
		{
			name:   "a client user cannot reach another workspace",
			actor:  &Actor{TenantID: clientA},
			tenant: clientB,
		},
		{
			// The important one. Staff reach is granted by IsStaff alone, so a
			// client-side actor carrying an assignment list — however it got
			// there — must still be confined to their own workspace.
			name:   "a client user is not helped by an assignment list",
			actor:  &Actor{TenantID: clientA, AssignedTenantIDs: []int64{clientB}},
			tenant: clientB,
		},
		{
			// A partner administers their own client, so they are the actor most
			// likely to be handed another client's slug by a stale link.
			name:   "a partner cannot reach another client",
			actor:  &Actor{TenantID: clientA, Roles: []string{"PARTNER"}},
			tenant: clientC,
		},
		{
			name:   "an agent reaches a client they own",
			actor:  &Actor{IsStaff: true, TenantID: karma, AssignedTenantIDs: []int64{clientA, clientB}},
			tenant: clientB,
			want:   true,
		},
		{
			// Ownership is responsibility, not permission: agents support every
			// client, and cannot administer one they are unable to see.
			name:   "an agent reaches a client they do not own",
			actor:  &Actor{IsStaff: true, TenantID: karma, AssignedTenantIDs: []int64{clientA}},
			tenant: clientB,
			want:   true,
		},
		{
			name:   "an agent owning nothing still reaches every client",
			actor:  &Actor{IsStaff: true, TenantID: karma},
			tenant: clientA,
			want:   true,
		},
		{
			name:   "an agent reaches their own platform tenant",
			actor:  &Actor{IsStaff: true, TenantID: karma},
			tenant: karma,
			want:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.actor.MayAccessTenant(tc.tenant); got != tc.want {
				t.Errorf("MayAccessTenant(%d) = %v, want %v", tc.tenant, got, tc.want)
			}
		})
	}
}
