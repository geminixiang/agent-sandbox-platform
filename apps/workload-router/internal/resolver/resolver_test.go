package resolver

import "testing"

func TestHTTPServiceTargetIsConstrained(t *testing.T) {
	target, err := NewHTTPServiceTarget("preview", "sandboxes", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if target.Hostname() != "preview.sandboxes.svc" || target.Authority() != "preview.sandboxes.svc:8080" {
		t.Fatalf("target = %q %q", target.Hostname(), target.Authority())
	}
	if !target.MatchesAuthority("PREVIEW.SANDBOXES.SVC:8080") {
		t.Fatal("authority comparison should be DNS-case-insensitive")
	}
	invalid := []struct {
		service   string
		namespace string
		port      uint16
	}{
		{"http://evil", "sandboxes", 80},
		{"preview/path", "sandboxes", 80},
		{"preview", "sandboxes?x=1", 80},
		{"127-0-0-1", "", 80},
		{"preview", "sandboxes", 0},
	}
	for _, value := range invalid {
		if _, err := NewHTTPServiceTarget(value.service, value.namespace, value.port); err == nil {
			t.Errorf("accepted target %#v", value)
		}
	}
	if err := (HTTPServiceTarget{}).Validate(); err == nil {
		t.Fatal("zero target was valid")
	}
}
