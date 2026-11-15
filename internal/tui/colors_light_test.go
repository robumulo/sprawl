package tui

import (
	"testing"

	"charm.land/lipgloss/v2"
)

func TestLightTerminal(t *testing.T) {
	cases := []struct {
		name, theme, fgbg string
		want             bool
	}{
		{"default is dark", "", "", false},
		{"explicit light", "light", "", true},
		{"explicit dark beats COLORFGBG", "dark", "0;15", false},
		{"COLORFGBG white bg", "", "0;15", true},
		{"COLORFGBG grey bg", "", "0;default;7", true},
		{"COLORFGBG black bg", "", "15;0", false},
		{"case and spaces", " Light ", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("SPRAWL_THEME", c.theme)
			t.Setenv("COLORFGBG", c.fgbg)
			if got := LightTerminal(); got != c.want {
				t.Fatalf("LightTerminal() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestNewThemeUsesLightPaletteOnLightTerminal(t *testing.T) {
	t.Setenv("SPRAWL_THEME", "light")
	t.Setenv("COLORFGBG", "")
	got := NewTheme("").Palette
	want := defaultLightPalette(got.Primary)
	if got != want {
		t.Fatalf("light terminal palette = %+v, want %+v", got, want)
	}
	if got.FgBase == lipgloss.Color("252") {
		t.Fatal("light palette must not use the dark theme's near-white body text")
	}
}
