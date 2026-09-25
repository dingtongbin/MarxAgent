// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"os"
	"testing"
)

// TestSandboxBackendAvailability fails instead of skipping when the CI matrix
// demands a native backend. Without it a missing platform backend would turn
// every isolation test into a silent skip on the very runners meant to prove
// the isolation works.
func TestSandboxBackendAvailability(t *testing.T) {
	if os.Getenv("MARXAGENT_REQUIRE_SANDBOX") != "1" {
		t.Skip("set MARXAGENT_REQUIRE_SANDBOX=1 to require a native sandbox backend")
	}
	if !platformAvailable() {
		t.Fatalf("the %s sandbox backend is required but unavailable", platformName())
	}
	if capabilities := platformCapabilities(); !capabilities.DefaultDeny {
		t.Fatalf("the %s sandbox backend does not default to deny", platformName())
	}
}
