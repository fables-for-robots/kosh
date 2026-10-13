// Derived from k9s's internal/config/templates/stock-skin.yaml, Copyright Authors of K9s, Apache-2.0 (see LICENSES/Apache-2.0.txt); changed for kosh.

package tui

import (
	"charm.land/lipgloss/v2"
	"github.com/fables-for-robots/kosh/render"
)

// The colours of k9s's stock skin (internal/config/templates/
// stock-skin.yaml at the reference commit), by the names it gives them.
var (
	colorAqua           = lipgloss.Color("#00FFFF")
	colorBlack          = lipgloss.Color("#000000")
	colorCadetBlue      = lipgloss.Color("#5F9EA0")
	colorDarkOrange     = lipgloss.Color("#FF8C00")
	colorDodgerBlue     = lipgloss.Color("#1E90FF")
	colorFuchsia        = lipgloss.Color("#FF00FF")
	colorGray           = lipgloss.Color("#808080")
	colorGreen          = lipgloss.Color("#008000")
	colorGreenYellow    = lipgloss.Color("#ADFF2F")
	colorLightSkyBlue   = lipgloss.Color("#87CEFA")
	colorLightSlateGray = lipgloss.Color("#778899")
	colorOrange         = lipgloss.Color("#FFA500")
	colorOrangeRed      = lipgloss.Color("#FF4500")
	colorPaleGreen      = lipgloss.Color("#98FB98")
	colorPapayaWhip     = lipgloss.Color("#FFEFD5")
	colorSeaGreen       = lipgloss.Color("#2E8B57")
	colorWhite          = lipgloss.Color("#FFFFFF")
)

// The styles every part draws with. They paint the foreground and leave
// the terminal's background alone, except under the cursor and a marked
// row.
var (
	// styleError is an error: the flash of a failure, a table's Err.
	styleError = lipgloss.NewStyle().Foreground(colorOrangeRed)
	// styleDim is what is there but not the point: a suggestion.
	styleDim = lipgloss.NewStyle().Foreground(colorGray)
	// styleTitle is a frame's title and the current crumb.
	styleTitle = lipgloss.NewStyle().Foreground(colorAqua).Bold(true)
	// styleKey is a key in the header and the help: "<ctrl-d>".
	styleKey = lipgloss.NewStyle().Foreground(colorDodgerBlue)
	// styleHint is what a key does, beside it.
	styleHint = lipgloss.NewStyle().Foreground(colorWhite)
	// styleCursor is the row under the cursor.
	styleCursor = lipgloss.NewStyle().Foreground(colorBlack).Background(colorAqua)
	// styleMatch is a match of a search in a text.
	styleMatch = lipgloss.NewStyle().Foreground(colorFuchsia).Bold(true).Underline(true)
)

// The styles of the frame's parts, which only this part of the UI uses.
var (
	styleText       = lipgloss.NewStyle().Foreground(colorCadetBlue)
	styleBorder     = lipgloss.NewStyle().Foreground(colorDodgerBlue)
	styleNumKey     = lipgloss.NewStyle().Foreground(colorFuchsia)
	styleInfoLabel  = lipgloss.NewStyle().Foreground(colorWhite)
	styleInfoValue  = lipgloss.NewStyle().Foreground(colorOrange)
	styleTitleOf    = lipgloss.NewStyle().Foreground(colorFuchsia).Bold(true)
	styleTitleCount = lipgloss.NewStyle().Foreground(colorPapayaWhip).Bold(true)
	styleTitleExtra = lipgloss.NewStyle().Foreground(colorSeaGreen)
	styleCrumb      = lipgloss.NewStyle().Foreground(colorCadetBlue)
	stylePromptCmd  = lipgloss.NewStyle().Foreground(colorAqua)
	stylePromptFind = lipgloss.NewStyle().Foreground(colorSeaGreen)
	stylePromptText = lipgloss.NewStyle().Foreground(colorCadetBlue)
	styleColumn     = lipgloss.NewStyle().Foreground(colorWhite).Bold(true)
	styleColumnAt   = lipgloss.NewStyle().Foreground(colorLightSkyBlue).Bold(true)
	styleSorter     = lipgloss.NewStyle().Foreground(colorAqua).Bold(true)
	styleMark       = lipgloss.NewStyle().Foreground(colorBlack).Background(colorPaleGreen)
	styleRowNew     = lipgloss.NewStyle().Foreground(colorDodgerBlue)
	styleRowChanged = lipgloss.NewStyle().Foreground(colorGreenYellow)
	styleSection    = lipgloss.NewStyle().Foreground(colorGreen).Bold(true)
)

// healthStyles colour a row by its health, as k9s's status colours do.
var healthStyles = map[render.Health]lipgloss.Style{
	render.OK:        lipgloss.NewStyle().Foreground(colorLightSkyBlue),
	render.Pending:   lipgloss.NewStyle().Foreground(colorDarkOrange),
	render.Error:     lipgloss.NewStyle().Foreground(colorOrangeRed),
	render.Completed: lipgloss.NewStyle().Foreground(colorLightSlateGray),
}

// styleOfHealth returns the style of a row of health h.
func styleOfHealth(h render.Health) lipgloss.Style {
	if s, ok := healthStyles[h]; ok {
		return s
	}
	return healthStyles[render.OK]
}
