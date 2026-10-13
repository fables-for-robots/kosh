package sshd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// longWait is the deadline of a wait for work that takes the machine
// real time.
const longWait = time.Minute

// The model of the hand-over's tests.

// handover is what a test of the hand-over shares with its model and
// with the function that gets the terminal.
type handover struct {
	// run gets the terminal when "!" is pressed.
	run func(context.Context, Terminal) error
	// atStart makes the model ask for the hand-over in Init.
	atStart bool
	// blocked gets a value when "b" was pressed and Update waits for a
	// value on release.
	blocked chan struct{}
	release chan struct{}

	mu     sync.Mutex
	keys   []string        // the keys the model was given
	pastes []string        // what was pasted
	marks  int             // mode reports the model was given
	sizes  int             // sizes the model was given
	w, h   int             // the last of them
	began  int             // calls of run
	ended  []error         // what the model's done was told
	begins []time.Time     // when each call of run began
	wake   chan<- struct{} // has a value put on it whenever something above changes
}

func newHandover(run func(context.Context, Terminal) error) *handover {
	return &handover{run: run, blocked: make(chan struct{}, 1), release: make(chan struct{})}
}

// note changes what the test looks at.
func (hv *handover) note(change func()) {
	hv.mu.Lock()
	change()
	wake := hv.wake
	hv.mu.Unlock()
	if wake != nil {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

// wait waits until cond, called with hv.mu held, holds.
func (hv *handover) wait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	wake := make(chan struct{}, 1)
	hv.mu.Lock()
	hv.wake = wake
	hv.mu.Unlock()
	deadline := time.NewTimer(longWait)
	defer deadline.Stop()
	for {
		hv.mu.Lock()
		ok := cond()
		hv.mu.Unlock()
		if ok {
			return
		}
		select {
		case <-wake:
		case <-deadline.C:
			hv.mu.Lock()
			defer hv.mu.Unlock()
			t.Fatalf("waited %v: %s; keys %q, began %d, ended %v, size %dx%d", longWait, what, hv.keys, hv.began, hv.ended, hv.w, hv.h)
		}
	}
}

// typed returns the keys the model was given, as one string.
func (hv *handover) typed() string {
	hv.mu.Lock()
	defer hv.mu.Unlock()
	return strings.Join(hv.keys, "")
}

type handoverDone struct{ err error }

// handoverModel draws a full screen. "!" hands the terminal to the
// test's run, "@" asks for that twice at once, "b" makes Update wait for
// the test, "q" quits.
type handoverModel struct {
	s    Session
	hv   *handover
	done int
	w, h int
}

func (m handoverModel) exec() tea.Cmd {
	hv := m.hv
	return m.s.Exec(func(ctx context.Context, t Terminal) error {
		hv.note(func() {
			hv.began++
			hv.begins = append(hv.begins, time.Now())
		})
		return hv.run(ctx, t)
	}, func(err error) tea.Msg { return handoverDone{err} })
}

func (m handoverModel) Init() tea.Cmd {
	if m.hv.atStart {
		return m.exec()
	}
	return nil
}

func (m handoverModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	hv := m.hv
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		hv.note(func() { hv.w, hv.h, hv.sizes = m.w, m.h, hv.sizes+1 })
	case tea.ModeReportMsg:
		hv.note(func() { hv.marks++ })
	case tea.PasteMsg:
		hv.note(func() { hv.pastes = append(hv.pastes, msg.Content) })
	case handoverDone:
		m.done++
		hv.note(func() { hv.ended = append(hv.ended, msg.err) })
	case tea.KeyPressMsg:
		k := msg.String()
		hv.note(func() { hv.keys = append(hv.keys, k) })
		switch k {
		case "!":
			return m, m.exec()
		case "@":
			return m, tea.Batch(m.exec(), m.exec())
		case "b":
			hv.blocked <- struct{}{}
			<-hv.release
		case "q":
			return m, tea.Quit
		}
	}
	return m, nil
}

// frame is what the model draws at w by h after done hand-overs.
func handoverFrame(done, w, h int) []string {
	rows := []string{fmt.Sprintf("HAND done:%d size:%dx%d", done, w, h)}
	for i := 1; i < max(h, 1)-1; i++ {
		rows = append(rows, fmt.Sprintf("row %02d %s", i, strings.Repeat("=", max(w-8, 0))))
	}
	return append(rows, "LASTLINE")
}

func (m handoverModel) View() tea.View {
	v := tea.NewView(strings.Join(handoverFrame(m.done, m.w, m.h), "\n"))
	v.AltScreen = true
	return v
}

// handoverHarness is a harness whose sessions have a handoverModel.
func handoverHarness(t *testing.T, hv *handover, opts ...option) *harness {
	t.Helper()
	return newHarness(t, append([]option{withConfig(func(c *Config) {
		c.Model = func(s Session) tea.Model { return handoverModel{s: s, hv: hv} }
	})}, opts...)...)
}

// onlySession returns the server's one session.
func (h *harness) onlySession() *session {
	h.t.Helper()
	h.srv.mu.Lock()
	defer h.srv.mu.Unlock()
	if len(h.srv.sessions) != 1 {
		h.t.Fatalf("the server has %d sessions, want 1", len(h.srv.sessions))
	}
	for sess := range h.srv.sessions {
		return sess
	}
	return nil
}

// readResult is what a Read returned.
type readResult struct {
	data string
	err  error
}

// readOnce reads r once, in a goroutine, and returns where the result
// will be.
func readOnce(r io.Reader) <-chan readResult {
	res := make(chan readResult, 1)
	go func() {
		b := make([]byte, 64)
		n, err := r.Read(b)
		res <- readResult{string(b[:n]), err}
	}()
	return res
}

// wantRead waits for the result of a readOnce and compares it.
func wantRead(t *testing.T, what string, res <-chan readResult, data string, err error) {
	t.Helper()
	select {
	case got := <-res:
		if got.data != data || got.err != err {
			t.Fatalf("%s: read %q, %v; want %q, %v", what, got.data, got.err, data, err)
		}
	case <-time.After(waitTime):
		t.Fatalf("%s: the read has not returned", what)
	}
}

func isClosed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// TestInputIsTheOneReadersWhoseTurnItIs follows a session's input
// through two hand-overs without a program: who is given a byte, and who
// is told that there are no more.
func TestInputIsTheOneReadersWhoseTurnItIs(t *testing.T) {
	channel, client := io.Pipe()
	in := newInput()
	pumped := make(chan struct{})
	go func() {
		in.pump(channel)
		close(pumped)
	}()
	program := programInput{in}
	send := func(s string) {
		t.Helper()
		if _, err := io.WriteString(client, s); err != nil {
			t.Fatal(err)
		}
	}

	send("ab")
	wantRead(t, "the program has the terminal", readOnce(program), "ab", nil)

	// The hand-over: the program's reader, which is waiting, gets the
	// mark and then its end, and nothing of what the client sends.
	waiting := readOnce(program)
	command, drained, told := in.give()
	if command == nil {
		t.Fatal("the terminal was not given")
	}
	wantRead(t, "the program's reader at the hand-over", waiting, drainMark, nil)
	if isClosed(told) {
		t.Error("the program's reader counts as told its end before it was")
	}
	send("cd")
	wantRead(t, "the program's reader after the mark", readOnce(program), "", io.EOF)
	if !isClosed(told) {
		t.Error("the program's reader was told its end, and that is not known")
	}
	if isClosed(drained) {
		t.Error("the mark counts as seen before the program's filter saw it")
	}
	in.markSeen()
	if !isClosed(drained) {
		t.Error("the mark was seen, and that is not known")
	}
	wantRead(t, "the command has the terminal", readOnce(command), "cd", nil)

	// One command at a time.
	if second, _, _ := in.give(); second != nil {
		t.Error("the terminal was given to a second command")
	}

	// The return: a Read of the command that is still waiting is told the
	// end, and what the client sends then is the program's.
	waiting = readOnce(command)
	in.takeBack(command)
	wantRead(t, "the command's reader at the return", waiting, "", io.EOF)
	send("ef")
	wantRead(t, "the command's reader after its turn", readOnce(command), "", io.EOF)
	wantRead(t, "the program has the terminal again", readOnce(program), "ef", nil)

	// A turn that is cut short: the command is told the end, and what it
	// has not read waits for the program.
	command, _, _ = in.give()
	wantRead(t, "the program's reader at the second hand-over", readOnce(program), drainMark, nil)
	send("gh")
	eventually(t, "the bytes are in", func() bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return len(in.buf) == 2
	})
	in.cut(command)
	wantRead(t, "the command's reader, cut short", readOnce(command), "", io.EOF)
	wantRead(t, "the program's reader while the turn is not over", readOnce(program), "", io.EOF)
	in.takeBack(command)
	wantRead(t, "the program after the turn that was cut short", readOnce(program), "gh", nil)

	// The end of the channel is every reader's end, and the pump's.
	_ = client.Close()
	wantRead(t, "the program at the channel's end", readOnce(program), "", io.EOF)
	select {
	case <-pumped:
	case <-time.After(waitTime):
		t.Fatal("the pump has not ended with the channel")
	}
	// No reader is left to tell: a hand-over does not wait for one.
	command, drained, told = in.give()
	if command == nil || !isClosed(drained) || !isClosed(told) {
		t.Error("a hand-over after the program's reader has ended waits for that reader")
	}
	wantRead(t, "the command at the channel's end", readOnce(command), "", io.EOF)
}

// TestInputEndsWhenItIsClosed closes an input whose channel stays open,
// as the session's end does: the readers are told the end, and the pump
// returns with the next thing the channel gives or fails with.
func TestInputEndsWhenItIsClosed(t *testing.T) {
	channel, client := io.Pipe()
	in := newInput()
	pumped := make(chan struct{})
	go func() {
		in.pump(channel)
		close(pumped)
	}()
	waiting := readOnce(programInput{in})
	in.close()
	wantRead(t, "the program's reader of a closed input", waiting, "", io.EOF)
	_ = client.Close()
	select {
	case <-pumped:
	case <-time.After(waitTime):
		t.Fatal("the pump has not ended")
	}
}

// TestInputIsBounded sends megabytes to a program that does not read:
// the server holds inputBuffer bytes of them and no more, the client is
// held up by SSH's flow control, and when the program reads again it
// gets every byte.
func TestInputIsBounded(t *testing.T) {
	hv := newHandover(nil)
	h := handoverHarness(t, hv)
	tm := openTerm(t, h.dial(h.key("ann", false), "u"))
	tm.waitFor("LASTLINE")
	sess := h.onlySession()

	// A paste, so that megabytes are one message for the model.
	const size = 16 << 20
	body := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	payload := append(append([]byte("\x1b[200~"), body...), "\x1b[201~"...)

	// The program does not read: its Update waits, and its read loop
	// waits for it with the key after.
	tm.press("b")
	select {
	case <-hv.blocked:
	case <-time.After(waitTime):
		t.Fatal("the model has not been given the key")
	}
	tm.press("x")
	release := sync.OnceFunc(func() { close(hv.release) })
	defer release()

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	var sent atomic.Int64
	wrote := make(chan error, 1)
	go func() {
		for rest := payload; len(rest) > 0; {
			n, err := tm.stdin.Write(rest[:min(len(rest), 16<<10)])
			sent.Add(int64(n))
			if err != nil {
				wrote <- err
				return
			}
			rest = rest[n:]
		}
		wrote <- nil
	}()

	// SSH's window is 2 MiB: when the client has sent half of that, what
	// the server was willing to take out of the channel it has taken.
	buffered := func() int {
		sess.in.mu.Lock()
		defer sess.in.mu.Unlock()
		return len(sess.in.buf)
	}
	eventuallyWithin(t, longWait, "the client has filled the channel's window", func() bool {
		return sent.Load() >= 1<<20 && buffered() >= inputBuffer
	})
	if n := buffered(); n != inputBuffer {
		t.Errorf("the server holds %d bytes of input, want its bound of %d", n, inputBuffer)
	}
	select {
	case err := <-wrote:
		t.Fatalf("the client sent all %d bytes to a program that does not read (%v)", len(payload), err)
	default:
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > size/2 {
		t.Errorf("the heap grew by %d bytes while %d were sent to a program that does not read", grown, sent.Load())
	}

	// The program reads again.
	release()
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("sending: %v", err)
		}
	case <-time.After(longWait):
		t.Fatalf("the client has sent %d of %d bytes", sent.Load(), len(payload))
	}
	hv.wait(t, "the model has the paste", func() bool { return len(hv.pastes) > 0 })
	hv.mu.Lock()
	got := hv.pastes[0]
	hv.mu.Unlock()
	if got != string(body) {
		t.Errorf("the model was pasted %d bytes, want the %d that were sent, the same", len(got), len(body))
	}
	if n := buffered(); n > inputBuffer {
		t.Errorf("the server holds %d bytes of input, more than its bound of %d", n, inputBuffer)
	}
}
