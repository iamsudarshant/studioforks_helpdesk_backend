package user

import "testing"

// The rank ladder decides who may reset whose password, so it is worth pinning
// directly rather than only through the API — where a reach or permission check
// often refuses first and hides whether the ladder would have.
func TestCanAdminister(t *testing.T) {
	cases := []struct {
		name   string
		actor  []string
		target []string
		want   bool
	}{
		{"super admin over a helpdesk head", []string{RoleSuperAdmin}, []string{RoleHelpdeskHead}, true},
		{"super admin over an employee", []string{RoleSuperAdmin}, []string{RoleEmployee}, true},
		{"head over an executive", []string{RoleHelpdeskHead}, []string{RoleHelpdeskExecutive}, true},
		{"executive over a client admin", []string{RoleHelpdeskExecutive}, []string{RoleClientAdmin}, true},
		{"client admin over an employee", []string{RoleClientAdmin}, []string{RoleEmployee}, true},

		// The whole point: equals and superiors are refused.
		{"head over another head", []string{RoleHelpdeskHead}, []string{RoleHelpdeskHead}, false},
		{"executive over another executive", []string{RoleHelpdeskExecutive}, []string{RoleHelpdeskExecutive}, false},
		{"employee over another employee", []string{RoleEmployee}, []string{RoleEmployee}, false},
		{"head over the super admin", []string{RoleHelpdeskHead}, []string{RoleSuperAdmin}, false},
		{"executive over a head", []string{RoleHelpdeskExecutive}, []string{RoleHelpdeskHead}, false},
		{"employee over anyone", []string{RoleEmployee}, []string{RoleClientExecutive}, false},

		// A deprecated key carries the authority of the role it maps to, so a
		// user who has not been migrated is neither over- nor under-powered.
		{"the old agent key ranks as an executive", []string{RoleAgent}, []string{RoleClientAdmin}, true},
		{"the old agent key is under a head", []string{RoleAgent}, []string{RoleHelpdeskHead}, false},
		{"the old partner key ranks as a client admin", []string{RolePartner}, []string{RoleEmployee}, true},
		{"the old super admin key still outranks all", []string{RoleKarmaSuperAdmin}, []string{RoleHelpdeskHead}, true},

		// Several roles: the strongest decides.
		{"strongest role wins for the actor", []string{RoleEmployee, RoleHelpdeskHead}, []string{RoleClientAdmin}, true},
		{"strongest role wins for the target", []string{RoleHelpdeskHead}, []string{RoleEmployee, RoleSuperAdmin}, false},

		// An unrecognised role ranks below everything, so it can be
		// administered but administers nobody — the safe direction to fail.
		{"an unknown role administers nobody", []string{"SOMETHING_NEW"}, []string{RoleEmployee}, false},
		{"an unknown role is administered by anyone", []string{RoleEmployee}, []string{"SOMETHING_NEW"}, true},
		{"no roles at all administers nobody", nil, []string{RoleEmployee}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanAdminister(tc.actor, tc.target); got != tc.want {
				t.Fatalf("CanAdminister(%v, %v) = %v, want %v", tc.actor, tc.target, got, tc.want)
			}
		})
	}
}

// Every canonical role has to appear in the ladder. One that does not ranks 0,
// which silently makes its holder unable to administer anybody — a permission
// failure that looks like a UI bug.
func TestEveryCanonicalRoleIsRanked(t *testing.T) {
	for _, key := range []string{
		RoleSuperAdmin, RoleHelpdeskHead, RoleHelpdeskExecutive,
		RoleClientAdmin, RoleClientExecutive, RoleEmployee,
	} {
		if RankOf([]string{key}) == 0 {
			t.Errorf("role %s has no rank", key)
		}
	}
}
