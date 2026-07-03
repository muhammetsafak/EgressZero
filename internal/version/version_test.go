package version

import "testing"

func TestVersionInjected(t *testing.T) {
	old := version
	defer func() { version = old }()

	version = "v9.9.9"
	if got := Version(); got != "v9.9.9" {
		t.Errorf("Version() = %q, want injected v9.9.9", got)
	}
}

func TestVersionFallbackNeverEmpty(t *testing.T) {
	old := version
	defer func() { version = old }()

	version = ""
	if got := Version(); got == "" {
		t.Error("Version() must never be empty")
	}
}
