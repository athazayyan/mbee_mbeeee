package identity

import (
	"slices"
	"testing"
)

func TestProfileRoles(t *testing.T) {
	business := membership{SubjectType: "business", Role: "owner"}
	organization := membership{SubjectType: "organization", Role: "member"}
	cases := []struct {
		memberships []membership
		admin       bool
		want        []string
	}{
		{nil, false, []string{}},
		{[]membership{business}, false, []string{"tenant"}},
		{[]membership{organization, organization}, false, []string{"organizer"}},
		{[]membership{business, organization}, true, []string{"tenant", "organizer", "admin"}},
		{nil, true, []string{"admin"}},
	}
	for _, tc := range cases {
		if got := profileRoles(tc.memberships, tc.admin); !slices.Equal(got, tc.want) {
			t.Errorf("want %v, got %v", tc.want, got)
		}
	}
}
