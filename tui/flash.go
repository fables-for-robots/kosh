package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// flashFor is how long a flash is shown.
const flashFor = 5 * time.Second

// flashMaxLines is how many lines an error in the flash is wrapped onto
// at most.
const flashMaxLines = 4

// flashDoneMsg says that the flash seq has been shown long enough, so
// that the screen is drawn again without it.
type flashDoneMsg struct{ seq int }

// flashLine is what the flash line says. An information is one line,
// shown for flashFor. An error is wrapped onto as many lines as it needs,
// up to flashMaxLines, and is shown for flashFor and then until a key is
// pressed, so that it is not gone before it was seen.
type flashLine struct {
	// text is cleaned; an information's is one line, an error keeps its
	// line feeds, where it is wrapped.
	text string
	err  bool
	at   time.Time
	seq  int
	// timeUp: the timer of the error shown has fired; keyed: a key was
	// pressed while it was shown.
	timeUp, keyed bool
	wrap          wrapped
}

// show makes msg the flash, from now.
func (f *flashLine) show(msg flashMsg, now time.Time) tea.Cmd {
	f.seq++
	f.err = msg.err
	if f.err {
		f.text = clean(msg.text)
	} else {
		f.text = cleanLine(msg.text)
	}
	f.at = now
	f.timeUp, f.keyed = false, false
	seq := f.seq
	return waitCmd(func() tea.Msg {
		time.Sleep(flashFor)
		return flashDoneMsg{seq}
	})
}

// done takes the flash away when msg is for the one shown, unless it is
// an error no key was pressed for yet.
func (f *flashLine) done(msg flashDoneMsg) {
	if msg.seq != f.seq {
		return
	}
	if f.err && !f.keyed {
		f.timeUp = true
		return
	}
	f.text = ""
}

// key says that a key was pressed: an error whose time is up goes.
func (f *flashLine) key() {
	if f.text == "" || !f.err {
		return
	}
	if f.timeUp {
		f.text = ""
		return
	}
	f.keyed = true
}

// lines draws the flash, w cells wide: one line, or for an error up to
// most lines. A flash older than flashFor by the clock is not drawn,
// unless it is an error no key was pressed for yet.
func (f *flashLine) lines(w, most int, now time.Time) []string {
	shown := f.text != "" && (now.Sub(f.at) < flashFor || f.err && !f.keyed)
	if !shown {
		return []string{fit("", w)}
	}
	if !f.err {
		return []string{styleText.Render(fit(f.text, w))}
	}
	ls := f.wrap.get(f.text, w, max(most, 1))
	if len(ls) == 0 {
		return []string{fit("", w)}
	}
	out := make([]string, len(ls))
	for i, l := range ls {
		out[i] = styleError.Render(fit(l, w))
	}
	return out
}
