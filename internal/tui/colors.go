package tui

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// Palette is the semantic color palette for the TUI (QUM-417). Every color
// used by the TUI should be sourced from a Palette role rather than a raw
// `lipgloss.Color("<ansi>")` literal, so the visual language is centralized
// and themable.
//
// Roles:
//   - Primary: user-configurable accent (tracks the accentColor arg to NewTheme).
//   - Accent: secondary accent (notifications, async messages).
//   - Success / Warning / Error / Info: standard semantic signals.
//   - Busy: amber/yellow used for "working but pinned" indicators.
//   - FgBase / FgSubtle / FgMostSubtle: foreground intensities.
//   - BgBase / BgLessVisible: panel and status-bar backgrounds.
//   - System: magenta used for system-injected viewport entries (inbox-drain
//     citations, system notices — QUM-338/QUM-590). Distinct from Primary/Accent
//     by design so the human watching can tell at a glance the system spoke.
type Palette struct {
	Primary       color.Color
	Accent        color.Color
	Success       color.Color
	Warning       color.Color
	Error         color.Color
	Info          color.Color
	Busy          color.Color
	System        color.Color
	FgBase        color.Color
	FgSubtle      color.Color
	FgMostSubtle  color.Color
	BgBase        color.Color
	BgLessVisible color.Color
	// QUM-664: visual-identity spike port roles.
	//   - UserPrompt: bold blue used for the `›` chevron prefix on user messages.
	//   - InputBar:   grey foreground used for the `▌` gutter on the input box.
	//   - InputBg:    dark chrome background that fills every textarea row so
	//     the gutter + textarea read as one continuous input box.
	UserPrompt color.Color
	InputBar   color.Color
	InputBg    color.Color
}

// defaultDarkPalette returns the default dark-terminal palette, with Primary
// bound to the caller-supplied accent. Color values preserve the pre-QUM-417
// pixel output so the migration is a pure refactor.
func defaultDarkPalette(accent color.Color) Palette {
	return Palette{
		Primary:       accent,
		Accent:        lipgloss.Color("51"),  // cyan (notifications)
		Success:       lipgloss.Color("42"),  // green
		Warning:       lipgloss.Color("214"), // amber
		Error:         lipgloss.Color("196"), // red
		Info:          lipgloss.Color("39"),  // cyan-blue
		Busy:          lipgloss.Color("220"), // yellow
		System:        lipgloss.Color("141"), // magenta (system/inbox-drain)
		FgBase:        lipgloss.Color("252"),
		FgSubtle:      lipgloss.Color("245"),
		FgMostSubtle:  lipgloss.Color("240"),
		BgBase:        lipgloss.Color("233"),
		BgLessVisible: lipgloss.Color("236"),
		// QUM-664: visual-identity spike palette roles.
		UserPrompt: lipgloss.Color("#22D3EE"), // cyan — dominant + distinct from white assistant text (QUM-677 S7 v4)
		InputBar:   lipgloss.Color("8"),       // grey vertical bar gutter
		InputBg:    lipgloss.Color("234"),     // dark chrome bg filling every input row
	}
}

// LightTerminal reports whether the TUI should use the light palette.
// SPRAWL_THEME=light|dark wins; otherwise COLORFGBG (set by many terminals as
// "<fg>;<bg>") is consulted, where a background of 7 or 15 means a light
// terminal. Anything else keeps the historical dark palette.
func LightTerminal() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SPRAWL_THEME"))) {
	case "light":
		return true
	case "dark":
		return false
	}
	if v := os.Getenv("COLORFGBG"); v != "" {
		parts := strings.Split(v, ";")
		switch parts[len(parts)-1] {
		case "7", "15":
			return true
		}
	}
	return false
}

// paletteFor picks the light or dark palette for the current terminal.
func paletteFor(accent color.Color) Palette {
	if LightTerminal() {
		return defaultLightPalette(accent)
	}
	return defaultDarkPalette(accent)
}

// defaultLightPalette mirrors defaultDarkPalette's roles for terminals with a
// light background: dark foregrounds, pale chrome backgrounds, and semantic
// hues deep enough to read on white.
func defaultLightPalette(accent color.Color) Palette {
	return Palette{
		Primary:       accent,
		Accent:        lipgloss.Color("30"),  // dark cyan (notifications)
		Success:       lipgloss.Color("28"),  // green
		Warning:       lipgloss.Color("166"), // dark orange
		Error:         lipgloss.Color("160"), // red
		Info:          lipgloss.Color("25"),  // blue
		Busy:          lipgloss.Color("136"), // dark yellow
		System:        lipgloss.Color("90"),  // purple (system/inbox-drain)
		FgBase:        lipgloss.Color("235"),
		FgSubtle:      lipgloss.Color("241"),
		FgMostSubtle:  lipgloss.Color("246"),
		BgBase:        lipgloss.Color("255"),
		BgLessVisible: lipgloss.Color("253"),
		UserPrompt:    lipgloss.Color("#0E7490"), // deep cyan chevron
		InputBar:      lipgloss.Color("248"),
		InputBg:       lipgloss.Color("254"),
	}
}
