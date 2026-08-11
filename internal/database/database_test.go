package database

import "testing"

func TestUserColumnsForQualifiedJoin(t *testing.T) {
	want := `u.id,u.username,u.display_name,COALESCE(u.email,''),u.role,u.disabled,u.created_at,COALESCE(u.last_login_at,'epoch')`
	if got := userColumnsFor("u"); got != want {
		t.Fatalf("userColumnsFor(\"u\") = %q, want %q", got, want)
	}
}
