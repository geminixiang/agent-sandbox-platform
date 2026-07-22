package runtime

import (
	"os"
	goruntime "runtime"
	"testing"
)

func TestLinuxSecurityIntegrationGateAvailability(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("Linux-only gate skipped: SO_PEERCRED, capabilities, no_new_privs, and Linux child credentials are unavailable on " + goruntime.GOOS)
	}
	if os.Geteuid() != 0 {
		t.Skip("Linux-only gate skipped: root is required to verify UID/GID 10001 child credentials and cross-UID process-group signalling")
	}
	// The concrete Linux checks are in linux_integration_test.go. This test only
	// makes platform gating visible rather than silently excluding those files.
}
