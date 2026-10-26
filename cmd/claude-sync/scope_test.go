package main

import (
	"testing"
	"unicode/utf8"

	"github.com/tawanorg/claude-sync/internal/config"
)

// TestResolveScope covers resolveScope: explicit valid values pass through,
// an empty value defaults to everything (every Mac ends up identical), and an
// unrecognized value is rejected.
func TestResolveScope(t *testing.T) {
	t.Run("valid values pass through unchanged", func(t *testing.T) {
		for _, in := range []string{config.ScopeEverything, config.ScopeFull, config.ScopeSessions} {
			got, err := resolveScope(in)
			if err != nil {
				t.Errorf("resolveScope(%q) returned error: %v", in, err)
			}
			if got != in {
				t.Errorf("resolveScope(%q) = %q, want %q", in, got, in)
			}
		}
	})

	t.Run("empty defaults to everything without prompting", func(t *testing.T) {
		got, err := resolveScope("")
		if err != nil || got != config.ScopeEverything {
			t.Errorf("resolveScope(\"\") = %q, %v; want %q", got, err, config.ScopeEverything)
		}
	})

	t.Run("unrecognized value is rejected", func(t *testing.T) {
		if _, err := resolveScope("bogus"); err == nil {
			t.Error("resolveScope(\"bogus\") expected an error, got nil")
		}
	})
}

// TestStradlLogoRowsAligned guards the banner wordmark against a stray
// character knocking one row out of line with the others.
func TestStradlLogoRowsAligned(t *testing.T) {
	if len(stradlLogo) == 0 {
		t.Fatal("empty logo")
	}
	width := utf8.RuneCountInString(stradlLogo[0])
	for i, row := range stradlLogo {
		if n := utf8.RuneCountInString(row); n != width {
			t.Errorf("logo row %d is %d columns wide, want %d", i, n, width)
		}
	}
	if len(logoGradient) < len(stradlLogo) {
		t.Errorf("gradient has %d colors for %d rows", len(logoGradient), len(stradlLogo))
	}
}
