package oidc

import "testing"

func TestDomainAllowed(t *testing.T) {
	if !domainAllowed("a@example.com", []string{"example.com"}) {
		t.Fatal("allowed domain rejected")
	}
	if domainAllowed("a@other.test", []string{"example.com"}) {
		t.Fatal("other domain accepted")
	}
	if !domainAllowed("a@anything.test", nil) {
		t.Fatal("empty allowlist should allow verified email")
	}
}
func TestSafeUsername(t *testing.T) {
	if got := safeUsername("Jane Doe!", "jane@example.com"); got != "jane-doe" {
		t.Fatalf("got %q", got)
	}
}
