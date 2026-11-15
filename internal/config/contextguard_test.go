// context_guard.tokens: the context-size threshold past which an agent is
// restarted into a fresh session. A STRING key on the idle_reclaim.* precedent,
// because absent and "0" must mean different things: absent is the default,
// "0" switches the guard off.
package config

import (
	"strings"
	"testing"
)

func TestContextGuardThreshold_UnsetIsTheDefault(t *testing.T) {
	got, err := (&Config{}).ContextGuardThreshold()
	if err != nil {
		t.Fatalf("ContextGuardThreshold() error = %v, want nil for an unset key", err)
	}
	if got != 120000 {
		t.Errorf("ContextGuardThreshold() = %d for an unset key, want 120000", got)
	}
}

func TestContextGuardThreshold_ExplicitZeroDisables(t *testing.T) {
	for _, raw := range []string{"0", " 0 "} {
		got, err := (&Config{ContextGuardTokens: raw}).ContextGuardThreshold()
		if err != nil {
			t.Fatalf("ContextGuardThreshold(%q) error = %v, want nil", raw, err)
		}
		if got != 0 {
			t.Errorf("ContextGuardThreshold(%q) = %d, want 0 (explicit zero disables the guard)", raw, got)
		}
	}
}

func TestContextGuardThreshold_ParsesExplicitValue(t *testing.T) {
	got, err := (&Config{ContextGuardTokens: "90000"}).ContextGuardThreshold()
	if err != nil {
		t.Fatalf("ContextGuardThreshold() error = %v", err)
	}
	if got != 90000 {
		t.Errorf("ContextGuardThreshold() = %d, want 90000", got)
	}
}

// A typo must be reported AND must not switch the guard off: an agent whose
// context then grows forever is the failure this key exists to prevent.
func TestContextGuardThreshold_UnparseableOrNegativeIsReportedAndKeepsTheDefault(t *testing.T) {
	for _, raw := range []string{"120k", "abc", "-5"} {
		got, err := (&Config{ContextGuardTokens: raw}).ContextGuardThreshold()
		if err == nil {
			t.Errorf("ContextGuardThreshold(%q) returned no error; a bad value must be reported", raw)
		} else if !strings.Contains(err.Error(), "context_guard.tokens") {
			t.Errorf("error should name the key, got: %v", err)
		}
		if got != DefaultContextGuardTokens {
			t.Errorf("ContextGuardThreshold(%q) = %d, want the default %d", raw, got, DefaultContextGuardTokens)
		}
	}
}

// An unquoted YAML integer must load into the string field: that is how a user
// will write it.
func TestContextGuardThreshold_LoadsUnquotedYAMLInteger(t *testing.T) {
	for raw, want := range map[string]int{"context_guard.tokens: 0\n": 0, "context_guard.tokens: 80000\n": 80000} {
		cfg, err := Load(writeConfig(t, raw))
		if err != nil {
			t.Fatalf("Load(%q): %v", raw, err)
		}
		got, err := cfg.ContextGuardThreshold()
		if err != nil || got != want {
			t.Errorf("Load(%q).ContextGuardThreshold() = (%d, %v), want (%d, nil)", raw, got, err, want)
		}
	}
}
