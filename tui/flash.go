package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// flashFor is how long a flash is shown.
const flashFor = 5 * time.Second

// flashDoneMsg says that the flash seq has been shown long enough, so
// that the screen is drawn again without it.
type flashDoneMsg struct{ seq int }

// flashLine is what the flash line says.
type flashLine struct {
	text string
	err  bool
	at   time.Time
	seq  int
}

// show makes msg the flash, from now.
func (f *flashLine) show(msg flashMsg, now time.Time) tea.Cmd {
	f.seq++
	f.text = cleanLine(msg.text)
	f.err = msg.err
	f.at = now
	seq := f.seq
	return waitCmd(func() tea.Msg {
		time.Sleep(flashFor)
		return flashDoneMsg{seq}
	})
}

// done takes the flash away when msg is for the one shown.
func (f *flashLine) done(msg flashDoneMsg) {
	if msg.seq == f.seq {
		f.text = ""
	}
}

// line draws the flash, w cells wide. A flash older than flashFor by the
// clock is not drawn.
func (f *flashLine) line(w int, now time.Time) string {
	if f.text == "" || now.Sub(f.at) >= flashFor {
		return fit("", w)
	}
	style := styleText
	if f.err {
		style = styleError
	}
	return style.Render(fit(f.text, w))
}
