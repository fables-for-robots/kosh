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
	// ask, when it is not nil, makes the model ask for the hand-over
	// when it has a value: a hand-over that no key asks for.
	ask chan struct{}
	// blocked gets a value when "%" was pressed and Update waits for a
	// value on release.
	blocked chan struct{}
	release chan struct{}

	mu      sync.Mutex
	session Session         // what the model was made from
	keys    []string        // the keys the model was given
	pastes  []string        // what was pasted
	marks   int             // mode reports the model was given
	sizes   int             // sizes the model was given
	w, h    int             // the last of them
	began   int             // calls of run
	cmd     []byte          // what echo read
	ended   []error         // what the model's done was told
	begins  []time.Time     // when each call of run began
	wake    chan<- struct{} // has a value put on it whenever something above changes
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
// test's run, "@" asks for that twice at once, "%" makes Update wait for
// the test, "q" quits.
type handoverModel struct {
	s    Session
	hv   *handover
	done int
	w, h int
}

// command is run, counted.
func (hv *handover) command(ctx context.Context, t Terminal) error {
	hv.note(func() {
		hv.began++
		hv.begins = append(hv.begins, time.Now())
	})
	return hv.run(ctx, t)
}

func (m handoverModel) exec() tea.Cmd {
	return m.s.Exec(m.hv.command, func(err error) tea.Msg { return handoverDone{err} })
}

type handoverAsked struct{}

func (m handoverModel) Init() tea.Cmd {
	switch {
	case m.hv.atStart:
		return m.exec()
	case m.hv.ask != nil:
		return func() tea.Msg {
			select {
			case <-m.hv.ask:
				return handoverAsked{}
			case <-m.s.Context.Done():
				return nil
			}
		}
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
	case handoverAsked:
		return m, m.exec()
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
		case "%":
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
		c.Model = func(s Session) tea.Model {
			hv.note(func() { hv.session = s })
			return handoverModel{s: s, hv: hv}
		}
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
	tm.press("%")
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

// What gets the terminal.

// endMark is the byte that ends echo.
const endMark = "#"

// echo is a command that reads its input byte by byte, so that it takes
// nothing after its end mark, and writes each byte back.
func (hv *handover) echo(_ context.Context, t Terminal) error {
	b := make([]byte, 1)
	for {
		n, err := t.In.Read(b)
		if n > 0 {
			hv.note(func() { hv.cmd = append(hv.cmd, b[0]) })
			if _, err := t.Out.Write(b[:n]); err != nil {
				return err
			}
			if string(b[:n]) == endMark {
				return nil
			}
		}
		if err != nil {
			return err
		}
	}
}

// held is a turn that a test has: the test does with the terminal what
// a command would, and ends the turn with finish.
type held struct {
	ctx    context.Context
	t      Terminal
	finish chan error
}

// hold returns a command that gives its turn to the test, and returns
// when the test finishes the turn or the turn's context is done.
func hold(turns chan held) func(context.Context, Terminal) error {
	return func(ctx context.Context, t Terminal) error {
		h := held{ctx, t, make(chan error, 1)}
		turns <- h
		select {
		case err := <-h.finish:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// next waits for the turn of a command made with hold.
func next(t *testing.T, turns chan held) held {
	t.Helper()
	select {
	case h := <-turns:
		return h
	case <-time.After(longWait):
		t.Fatal("no command was given the terminal")
		return held{}
	}
}

// startHandover starts a server whose sessions have hv's model, and a
// session on a terminal of 80 by 24 that has drawn its first frame.
func startHandover(t *testing.T, hv *handover, opts ...option) (*harness, *term) {
	t.Helper()
	h := handoverHarness(t, hv, opts...)
	tm := openTerm(t, h.dial(h.key("ann", false), "u"))
	if !hv.atStart {
		tm.waitFor("LASTLINE")
	}
	return h, tm
}

// The bytes typed in each phase of a cycle of TestHandoverLosesNothing;
// no byte is in two of them.
const (
	bytesDuring  = "klmn"     // right after the key that asks for the hand-over
	bytesCommand = "ABCDEFGH" // once the command has begun
	bytesReturn  = "wyzv"     // right after the command's end mark
	bytesProgram = "01234567" // once the model has been told that the command ended
)

// handoverVariant is how TestHandoverLosesNothing types.
type handoverVariant struct {
	name string
	// pause is the time between two bytes.
	pause time.Duration
	// during types bytesDuring and bytesReturn too, while the terminal
	// changes hands.
	during bool
	// procs, when it is not 0, is GOMAXPROCS for the variant.
	procs int
}

// TestHandoverLosesNothing hands the terminal over and takes it back,
// with the client typing across both seams, one byte per SSH write. In
// every cycle each byte went to the one it was meant for, once, in the
// order typed: to the command what was typed once it had begun, the
// first byte included; to the model what was typed once it knew of the
// command's end, the first key included. What is typed while the
// terminal changes hands goes to the program up to some byte and to the
// other from that byte on, and none of it is lost either.
func TestHandoverLosesNothing(t *testing.T) {
	const cycles = 200
	variants := []handoverVariant{
		{name: "no pause"},
		{name: "a millisecond between bytes", pause: time.Millisecond},
		{name: "typing while the terminal changes hands", during: true},
		{name: "typing while the terminal changes hands, a millisecond between bytes", during: true, pause: time.Millisecond},
		{name: "GOMAXPROCS=1", during: true, procs: 1},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			if v.procs != 0 {
				before := runtime.GOMAXPROCS(v.procs)
				defer runtime.GOMAXPROCS(before)
			}
			hv := newHandover(nil)
			hv.run = hv.echo
			_, tm := startHandover(t, hv)
			typeBytes := func(s string) {
				for i := range len(s) {
					tm.press(s[i : i+1])
					if v.pause > 0 {
						time.Sleep(v.pause)
					}
				}
			}
			var (
				handover, handoverMax time.Duration
				duringToProgram       int // of bytesDuring
				returnToCommand       int // of bytesReturn
			)
			for i := 1; i <= cycles; i++ {
				hv.note(func() { hv.keys, hv.cmd = nil, nil })
				sent := "!"
				asked := time.Now()
				tm.press("!")
				if v.during {
					typeBytes(bytesDuring)
					sent += bytesDuring
				}
				hv.wait(t, fmt.Sprintf("cycle %d: the command has begun", i), func() bool { return hv.began == i })
				hv.mu.Lock()
				d := hv.begins[i-1].Sub(asked)
				hv.mu.Unlock()
				handover += d
				handoverMax = max(handoverMax, d)

				typeBytes(bytesCommand)
				tm.press(endMark)
				sent += bytesCommand + endMark
				if v.during {
					typeBytes(bytesReturn)
					sent += bytesReturn
				}
				hv.wait(t, fmt.Sprintf("cycle %d: the model knows that the command ended", i), func() bool { return len(hv.ended) == i })
				typeBytes(bytesProgram)
				sent += bytesProgram

				// Every byte has arrived, at the program or at the command.
				hv.wait(t, fmt.Sprintf("cycle %d: every byte of %q has arrived", i, sent), func() bool {
					return len(hv.keys)+len(hv.cmd) >= len(sent)
				})
				hv.mu.Lock()
				program, command := strings.Join(hv.keys, ""), string(hv.cmd)
				hv.mu.Unlock()

				// What the program must have: the key that asked; of the
				// bytes typed during the hand-over the first so many; of
				// those typed during the return the last so many; all of
				// those typed after it. The command must have the rest.
				during, returned := "", ""
				if v.during {
					during, returned = bytesDuring, bytesReturn
				}
				k := 0
				for k < len(during) && strings.HasPrefix(program, "!"+during[:k+1]) {
					k++
				}
				j := 0
				for j < len(returned) && !strings.HasSuffix(program, returned[j:]+bytesProgram) {
					j++
				}
				wantProgram := "!" + during[:k] + returned[j:] + bytesProgram
				wantCommand := during[k:] + bytesCommand + endMark + returned[:j]
				if program != wantProgram || command != wantCommand {
					t.Fatalf("cycle %d: typed %q\nthe program got %q\nthe command got %q\nwhich is not each byte once, in order, where it was meant to go", i, sent, program, command)
				}
				duringToProgram += k
				returnToCommand += j
			}
			t.Logf("%d cycles, %d bytes each: none lost, none twice, none out of order", cycles, len("!")+len(bytesCommand)+len(endMark)+len(bytesProgram)+map[bool]int{true: len(bytesDuring) + len(bytesReturn)}[v.during])
			if v.during {
				n := cycles * len(bytesDuring)
				t.Logf("typed during the hand-over: %d bytes, %d to the program, %d to the command", n, duringToProgram, n-duringToProgram)
				n = cycles * len(bytesReturn)
				t.Logf("typed during the return: %d bytes, %d to the command, %d to the program", n, returnToCommand, n-returnToCommand)
				t.Logf("from the key to the command's beginning, the typing included: mean %v, at most %v",
					(handover / cycles).Round(time.Microsecond), handoverMax.Round(time.Microsecond))
			} else {
				t.Logf("from the key to the command's beginning: mean %v, at most %v",
					(handover / cycles).Round(time.Microsecond), handoverMax.Round(time.Microsecond))
			}
		})
	}
}

// TestHandoverIsFast guards against the half second that Bubble Tea
// waits for a read loop that does not end: the time from the key to the
// command's beginning.
func TestHandoverIsFast(t *testing.T) {
	const cycles = 50
	hv := newHandover(nil)
	hv.run = hv.echo
	_, tm := startHandover(t, hv)
	var total time.Duration
	for i := 1; i <= cycles; i++ {
		asked := time.Now()
		tm.press("!")
		hv.wait(t, "the command has begun", func() bool { return hv.began == i })
		hv.mu.Lock()
		total += hv.begins[i-1].Sub(asked)
		hv.mu.Unlock()
		tm.press(endMark)
		hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == i })
	}
	mean := total / cycles
	t.Logf("from the key to the command's beginning: mean %v over %d hand-overs", mean.Round(time.Microsecond), cycles)
	if mean >= 50*time.Millisecond {
		t.Errorf("a hand-over takes %v on average, want less than 50ms", mean)
	}
}

// bareLineFeeds returns how many line feeds of s have no carriage return
// before them, and how many line feeds s has.
func bareLineFeeds(s string) (bare, all int) {
	for i := range len(s) {
		if s[i] == '\n' {
			all++
			if i == 0 || s[i-1] != '\r' {
				bare++
			}
		}
	}
	return bare, all
}

// TestCommandOutputIsRaw writes every byte there is, and bare line
// feeds, to the command's terminal: the client receives them as they
// were written. What the program writes before and after still has
// every line feed made a carriage return and a line feed, which its
// renderer counts on.
func TestCommandOutputIsRaw(t *testing.T) {
	payload := []byte("a\nb\n")
	for b := range 256 {
		payload = append(payload, byte(b))
	}
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	_, tm := startHandover(t, hv)
	before := tm.stdout.String()

	tm.press("!")
	turn := next(t, turns)
	if n, err := turn.t.Out.Write(payload); n != len(payload) || err != nil {
		t.Fatalf("writing to the command's terminal: %d, %v", n, err)
	}
	tm.waitFor(string(payload))
	turn.finish <- nil
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	tm.waitFor("done:1")
	out := tm.stdout.String()

	if n := strings.Count(out, string(payload)); n != 1 {
		t.Errorf("the client received what the command wrote %d times", n)
	}
	at := strings.Index(out, string(payload))
	after := out[at+len(payload):]
	for what, s := range map[string]string{"before the command": before, "after the command": after} {
		bare, all := bareLineFeeds(s)
		if all == 0 || bare != 0 {
			t.Errorf("the program's output %s has %d line feeds, %d of them without a carriage return; want some, and none bare", what, all, bare)
		}
	}
}

// screenOf returns the screen of an 80 by 24 terminal that was sent what
// the session has written.
func screenOf(tm *term) *screen {
	return feed(80, 24, tm.stdout.String())
}

// TestScreenAfterTheReturn lets a command scribble on the terminal:
// after the return the screen is the model's frame, every row of it, and
// the model has done nothing for that.
func TestScreenAfterTheReturn(t *testing.T) {
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	_, tm := startHandover(t, hv)
	want := strings.Join(handoverFrame(0, 80, 24), "\n")
	eventually(t, "the model's first frame is on the screen", func() bool { return screenOf(tm).String() == want })

	tm.press("!")
	turn := next(t, turns)
	// On the normal screen, where the command is: lines that scroll,
	// and a cursor left somewhere.
	scribble := strings.Repeat("the command was here\r\n", 30) + "\x1b[5;7Hand here"
	if _, err := io.WriteString(turn.t.Out, scribble); err != nil {
		t.Fatal(err)
	}
	tm.waitFor("and here")
	if got := screenOf(tm).String(); !strings.Contains(got, "the command was here") || strings.Contains(got, "LASTLINE") {
		t.Errorf("while the command has the terminal the screen is not the command's:\n%s", got)
	}
	turn.finish <- nil

	want = strings.Join(handoverFrame(1, 80, 24), "\n")
	eventually(t, "the model's frame is on the screen after the return", func() bool { return screenOf(tm).String() == want })
	if got := screenOf(tm).String(); got != want {
		t.Errorf("the screen after the return:\n%s\nwant:\n%s", got, want)
	}
}

// TestCommandSizes changes the window while a command has the terminal.
func TestCommandSizes(t *testing.T) {
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	h, tm := startHandover(t, hv)
	sess := h.onlySession()
	// windowIs waits until the server knows of a change, without asking
	// as the command does.
	windowIs := func(w, hh int) {
		t.Helper()
		eventuallyWithin(t, longWait, fmt.Sprintf("the window is %dx%d", w, hh), func() bool {
			sess.win.mu.Lock()
			defer sess.win.mu.Unlock()
			return sess.win.size.Width == w && sess.win.size.Height == hh
		})
	}
	change := func(w, hh int) {
		t.Helper()
		if err := tm.sess.WindowChange(hh, w); err != nil {
			t.Fatal(err)
		}
	}
	resized := func(turn held) bool {
		select {
		case <-turn.t.Resized:
			return true
		default:
			return false
		}
	}
	wantSize := func(turn held, w, hh int) {
		t.Helper()
		if gw, gh := turn.t.Size(); gw != w || gh != hh {
			t.Errorf("Size returns %dx%d, want %dx%d", gw, gh, w, hh)
		}
	}

	// A change before the command is not the command's to hear of.
	change(90, 25)
	hv.wait(t, "the model has 90x25", func() bool { return hv.w == 90 && hv.h == 25 })

	tm.press("!")
	turn := next(t, turns)
	if resized(turn) {
		t.Error("Resized has a value at the start of the turn")
	}
	wantSize(turn, 90, 25)

	// One change: one value, and the new size.
	change(100, 30)
	select {
	case <-turn.t.Resized:
	case <-time.After(longWait):
		t.Fatal("Resized has no value after the window changed")
	}
	wantSize(turn, 100, 30)
	if resized(turn) {
		t.Error("Resized has a second value for one change")
	}

	// Three changes while the command does not look: one value.
	change(110, 35)
	change(120, 40)
	change(130, 45)
	windowIs(130, 45)
	if !resized(turn) {
		t.Error("Resized has no value after three changes")
	}
	if resized(turn) {
		t.Error("Resized has more than one value after three changes")
	}
	wantSize(turn, 130, 45)

	// A change that Size has told the command of already is not news.
	change(140, 50)
	windowIs(140, 50)
	wantSize(turn, 140, 50)
	if resized(turn) {
		t.Error("Resized has a value for a size that Size has returned already")
	}

	// The program is given no size while the command runs, and has the
	// last one after it.
	turn.finish <- nil
	hv.wait(t, "the model has the size that holds after the turn", func() bool { return hv.w == 140 && hv.h == 50 })

	// After the turn the command hears of no change.
	change(80, 24)
	hv.wait(t, "the model has 80x24", func() bool { return hv.w == 80 && hv.h == 24 })
	if resized(turn) {
		t.Error("Resized has a value for a change after the turn")
	}
}

// TestReaderThatOutlivesItsTurn leaves a goroutine of the command in
// In.Read when the command returns, as a copier of a stream is left: it
// is told io.EOF, and takes nothing of what the client types then. What
// the command writes after its turn is refused.
func TestReaderThatOutlivesItsTurn(t *testing.T) {
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	_, tm := startHandover(t, hv)

	tm.press("!")
	turn := next(t, turns)
	left := readOnce(turn.t.In)
	select {
	case got := <-left:
		t.Fatalf("the command read %q, %v when the client had sent nothing", got.data, got.err)
	default:
	}
	turn.finish <- nil
	wantRead(t, "the reader that outlived its turn", left, "", io.EOF)
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })

	tm.press("x")
	hv.wait(t, "the model has the key typed after the return", func() bool { return strings.HasSuffix(strings.Join(hv.keys, ""), "x") })
	wantRead(t, "the command's reader after the client typed", readOnce(turn.t.In), "", io.EOF)

	before := tm.stdout.String()
	if n, err := io.WriteString(turn.t.Out, "LATEWRITE"); n != 0 || err != io.ErrClosedPipe {
		t.Errorf("a write after the turn: %d, %v; want 0, io.ErrClosedPipe", n, err)
	}
	// Something of the program's, after which the late write would be
	// there if it had been let through.
	if err := tm.sess.WindowChange(30, 100); err != nil {
		t.Fatal(err)
	}
	tm.waitFor("size:100x30")
	if out := tm.stdout.String()[len(before):]; strings.Contains(out, "LATEWRITE") {
		t.Error("the client received what the command wrote after its turn")
	}
}

// TestTheMarkIsSwallowed has a client send the mark that ends the
// program's reading before a hand-over: the model is not given it, the
// keys around it arrive, and a hand-over after it works.
func TestTheMarkIsSwallowed(t *testing.T) {
	hv := newHandover(nil)
	hv.run = hv.echo
	_, tm := startHandover(t, hv)

	// A report of another mode is the model's, as it was.
	tm.press("\x1b[?2026;2$y")
	hv.wait(t, "the model has the report of a mode that is not the mark's", func() bool { return hv.marks == 1 })

	tm.press("a" + drainMark + "b")
	hv.wait(t, "the model has the keys around the mark", func() bool { return strings.Join(hv.keys, "") == "ab" })
	if hv.marks != 1 {
		t.Errorf("the model was given the mark")
	}

	tm.press("!")
	hv.wait(t, "the command has begun", func() bool { return hv.began == 1 })
	tm.press("A" + endMark)
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	tm.press("c")
	hv.wait(t, "the model has the key typed after the return", func() bool { return strings.Join(hv.keys, "") == "ab!c" })
	hv.mu.Lock()
	defer hv.mu.Unlock()
	if string(hv.cmd) != "A"+endMark || hv.marks != 1 || hv.ended[0] != nil {
		t.Errorf("the command read %q and returned %v, the model was given %d mode reports", hv.cmd, hv.ended[0], hv.marks)
	}
}

// TestHandoverAtTheStart asks for the hand-over in the model's Init,
// before the client's terminal has answered what Bubble Tea asks it at
// the start. Nothing special is done for that: an answer that arrives
// after the hand-over is the command's, like every byte, and the program
// goes without it.
func TestHandoverAtTheStart(t *testing.T) {
	hv := newHandover(nil)
	hv.run = hv.echo
	hv.atStart = true
	_, tm := startHandover(t, hv)
	hv.wait(t, "the command has begun", func() bool { return hv.began == 1 })
	// What the program asked the terminal: whether it has the mode of
	// synchronized output.
	const query, reply = "\x1b[?2026$p", "\x1b[?2026;2$y"
	tm.waitFor(query)
	tm.press(reply)
	tm.press(endMark)
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	tm.press("x")
	hv.wait(t, "the model has the key typed after the return", func() bool { return len(hv.keys) > 0 })
	tm.waitFor("done:1")

	hv.mu.Lock()
	defer hv.mu.Unlock()
	if string(hv.cmd) != reply+endMark {
		t.Errorf("the command read %q, want the terminal's answer and the end mark, %q", hv.cmd, reply+endMark)
	}
	if strings.Join(hv.keys, "") != "x" || hv.marks != 0 {
		t.Errorf("the model was given the keys %q and %d mode reports, want the key x and no report", hv.keys, hv.marks)
	}
}

// TestHandoverInTheMiddleOfAPaste asks for the hand-over when the
// program's reader has the start of a paste and not its end: the mark
// cannot come out of that reader, and the hand-over goes on without it
// after drainWait. From then on the client's bytes are the command's.
func TestHandoverInTheMiddleOfAPaste(t *testing.T) {
	hv := newHandover(nil)
	hv.run = hv.echo
	hv.ask = make(chan struct{}, 1)
	_, tm := startHandover(t, hv)

	// One SSH write, so one read of the program's reader: when the model
	// has the key, the reader has the start of the paste.
	tm.press("z\x1b[200~pasted")
	hv.wait(t, "the model has the key before the paste", func() bool { return len(hv.keys) == 1 })

	asked := time.Now()
	hv.ask <- struct{}{}
	hv.wait(t, "the command has begun", func() bool { return hv.began == 1 })
	hv.mu.Lock()
	took := hv.begins[0].Sub(asked)
	hv.mu.Unlock()
	if took < drainWait {
		t.Errorf("the hand-over took %v: it did not wait %v for the mark, so the mark came out", took, drainWait)
	}
	tm.press("AB" + endMark)
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	tm.press("c")
	hv.wait(t, "the model has the key typed after the return", func() bool { return strings.HasSuffix(strings.Join(hv.keys, ""), "c") })

	hv.mu.Lock()
	defer hv.mu.Unlock()
	if string(hv.cmd) != "AB"+endMark || hv.marks != 0 {
		t.Errorf("the command read %q, the model was given %d mode reports", hv.cmd, hv.marks)
	}
	t.Logf("the hand-over took %v; of the paste that did not end the model was given the keys %q and the pastes %q", took.Round(time.Millisecond), hv.keys, hv.pastes)
}

// TestOneCommandAtATime asks for the terminal twice in one update: one
// command gets it, and the other is refused without being called.
func TestOneCommandAtATime(t *testing.T) {
	turns := make(chan held, 2)
	hv := newHandover(hold(turns))
	_, tm := startHandover(t, hv)

	tm.press("@")
	turn := next(t, turns)
	turn.finish <- nil
	hv.wait(t, "the model was told of both", func() bool { return len(hv.ended) == 2 })
	hv.mu.Lock()
	began, ended := hv.began, append([]error(nil), hv.ended...)
	hv.mu.Unlock()
	if began != 1 {
		t.Errorf("%d commands were given the terminal, want 1", began)
	}
	if !(ended[0] == nil && ended[1] == ErrTerminalBusy) && !(ended[0] == ErrTerminalBusy && ended[1] == nil) {
		t.Errorf("the model was told %v, want nil for the command that ran and ErrTerminalBusy for the other", ended)
	}

	// The terminal is free again.
	tm.press("!")
	turn = next(t, turns)
	turn.finish <- nil
	hv.wait(t, "the model was told of the third", func() bool { return len(hv.ended) == 3 })
	if hv.ended[2] != nil {
		t.Errorf("the command after the refused one: %v", hv.ended[2])
	}
}

// TestSessionEndsDuringACommand ends the session, in each of the ways a
// session ends, while a command has the terminal and waits in In.Read:
// the command's context is done at once, its Read returns io.EOF, its
// writes fail, and the session ends as it would have without a command.
func TestSessionEndsDuringACommand(t *testing.T) {
	type ending struct {
		name string
		// end ends the session.
		end func(t *testing.T, h *harness, cl interface{ Close() error }, tm *term)
		// reason is the reason of the session's end in the log.
		reason string
		// msg is what the client is told, or "" when it cannot be told.
		msg string
	}
	endings := []ending{
		{name: "the client closes its connection",
			end: func(_ *testing.T, _ *harness, cl interface{ Close() error }, _ *term) { _ = cl.Close() }},
		{name: "the client closes the channel", reason: reasonChannelClosed,
			end: func(_ *testing.T, _ *harness, _ interface{ Close() error }, tm *term) { _ = tm.sess.Close() }},
		{name: "the key is removed", reason: MsgKeyRemoved, msg: MsgKeyRemoved,
			end: func(t *testing.T, h *harness, _ interface{ Close() error }, _ *term) { setKeys(t, h.list) }},
		{name: "Shutdown", reason: MsgShuttingDown, msg: MsgShuttingDown,
			end: func(t *testing.T, h *harness, _ interface{ Close() error }, _ *term) {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), longWait)
					defer cancel()
					if err := h.srv.Shutdown(ctx); err != nil {
						t.Errorf("Shutdown: %v", err)
					}
				}()
			}},
	}
	for _, e := range endings {
		t.Run(e.name, func(t *testing.T) {
			// The command, as a careless one is: it waits for its
			// context only, with a goroutine of its own in Read.
			type running struct {
				ctx  context.Context
				t    Terminal
				read <-chan readResult
			}
			runs := make(chan running, 1)
			returned := make(chan struct{})
			hv := newHandover(func(ctx context.Context, tt Terminal) error {
				defer close(returned)
				runs <- running{ctx, tt, readOnce(tt.In)}
				<-ctx.Done()
				return ctx.Err()
			})
			h := handoverHarness(t, hv)
			cl := h.dial(h.key("ann", false), "u")
			tm := openTerm(t, cl)
			tm.waitFor("LASTLINE")
			tm.press("!")
			var run running
			select {
			case run = <-runs:
			case <-time.After(longWait):
				t.Fatal("the command was not given the terminal")
			}

			ended := time.Now()
			e.end(t, h, cl, tm)
			select {
			case <-run.ctx.Done():
				if d := time.Since(ended); d > 100*time.Millisecond {
					t.Errorf("the command's context was done %v after the session was ended, want within 100ms", d)
				}
			case <-time.After(longWait):
				t.Fatal("the command's context is not done")
			}
			wantRead(t, "the command's Read that was waiting", run.read, "", io.EOF)
			select {
			case <-returned:
			case <-time.After(longWait):
				t.Fatal("the command has not returned")
			}
			if n, err := io.WriteString(run.t.Out, "LATEWRITE"); n != 0 || err != io.ErrClosedPipe {
				t.Errorf("a write of the command after the session's end: %d, %v; want 0, io.ErrClosedPipe", n, err)
			}

			rec := h.log.waitFor(t, "of the session's end", func(r logRecord) bool { return r.Msg == "session end" })
			// A connection that is closed closes its channels: which of
			// the two the session saw first is the scheduler's choice,
			// with a command and without.
			either := e.reason == "" && rec.Attrs["reason"] == reasonChannelClosed
			if rec.Attrs["reason"] != e.reason && !either {
				t.Errorf("the session's end is logged with the reason %q, want %q", rec.Attrs["reason"], e.reason)
			}
			if e.msg != "" {
				if status := tm.exit(); status != 1 {
					t.Errorf("exit status %d, want 1", status)
				}
				out := tm.stdout.String()
				if !strings.HasSuffix(out, e.msg+"\r\n") {
					t.Errorf("the client was not told %q at the end; the session's last bytes are %q", e.msg, out[max(0, len(out)-100):])
				}
				if strings.Contains(out, "LATEWRITE") {
					t.Error("the client received what the command wrote after the session's end")
				}
			}
			if _, ok := h.log.find(func(r logRecord) bool { return strings.HasPrefix(r.Msg, "session did not end in time") }); ok {
				t.Errorf("the session had to be cut off:\n%s", h.log.dump())
			}
			eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
		})
	}
}

// TestCommandThatDoesNotReturn has a command that ignores its context:
// kosh's end of the session is not held up by it for longer than the
// end's timeout, after which the connection is closed and the log says
// why. The session's goroutines are there until the command returns, as
// they are for a program that does not return.
func TestCommandThatDoesNotReturn(t *testing.T) {
	stuck := make(chan struct{})
	unstick := sync.OnceFunc(func() { close(stuck) })
	defer unstick()
	runs := make(chan context.Context, 1)
	hv := newHandover(func(ctx context.Context, _ Terminal) error {
		runs <- ctx
		<-stuck
		return nil
	})
	h := handoverHarness(t, hv, withServer(func(s *Server) { s.endTimeout = 200 * time.Millisecond }))
	cl := h.dial(h.key("ann", false), "u")
	tm := openTerm(t, cl)
	tm.waitFor("LASTLINE")
	tm.press("!")
	var ctx context.Context
	select {
	case ctx = <-runs:
	case <-time.After(longWait):
		t.Fatal("the command was not given the terminal")
	}

	setKeys(t, h.list)
	select {
	case <-ctx.Done():
	case <-time.After(longWait):
		t.Fatal("the command's context is not done")
	}
	rec := h.log.waitFor(t, "of the session that did not end", func(r logRecord) bool {
		return strings.HasPrefix(r.Msg, "session did not end in time")
	})
	if !strings.Contains(rec.Msg, "did not return") || rec.Level.String() != "WARN" || rec.Attrs["key"] != "ann" || rec.Attrs["reason"] != MsgKeyRemoved {
		t.Errorf("the line %v does not say that the command did not return, or whose session it was and why it was ended", rec)
	}
	// The connection is closed: the client's session ends without an
	// exit status, and the connection with it.
	closed := make(chan error, 1)
	go func() { closed <- cl.Wait() }()
	select {
	case <-closed:
	case <-time.After(longWait):
		t.Fatal("the connection of the session that did not end is open")
	}
	select {
	case err := <-tm.done:
		if err == nil {
			t.Error("the session ended with exit status 0")
		}
	case <-time.After(longWait):
		t.Fatal("the client's session has not ended")
	}

	// When the command does return, nothing of the session is left.
	unstick()
	h.log.waitFor(t, "of the session's end", func(r logRecord) bool { return r.Msg == "session end" && r.Attrs["reason"] == MsgKeyRemoved })
	eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
}

// TestExecAfterTheEnd runs the command of a hand-over when its session
// has ended: the model's done is told so, and run is not called.
func TestExecAfterTheEnd(t *testing.T) {
	hv := newHandover(nil)
	hv.run = hv.echo
	h, tm := startHandover(t, hv)
	hv.mu.Lock()
	s := hv.session
	hv.mu.Unlock()
	called := false
	late := s.Exec(func(context.Context, Terminal) error {
		called = true
		return nil
	}, func(err error) tea.Msg { return handoverDone{err} })

	tm.press("q")
	if status := tm.exit(); status != 0 {
		t.Fatalf("exit status %d", status)
	}
	contextEnds(t, s, "after the model quit")
	eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })

	got := make(chan tea.Msg, 1)
	go func() { got <- late() }()
	select {
	case msg := <-got:
		if done, ok := msg.(handoverDone); !ok || done.err != ErrSessionEnded {
			t.Errorf("the command of a session that has ended returned %#v, want done's message for ErrSessionEnded", msg)
		}
	case <-time.After(waitTime):
		t.Fatal("the command of a session that has ended does not return")
	}
	if called {
		t.Error("run was called for a session that has ended")
	}
}

// TestExecWhenTheSessionIsBeingEnded runs the command of a hand-over
// when kosh has ended the session and the program has not stopped yet.
func TestExecWhenTheSessionIsBeingEnded(t *testing.T) {
	hv := newHandover(nil)
	hv.run = hv.echo
	h, tm := startHandover(t, hv)
	hv.mu.Lock()
	s := hv.session
	hv.mu.Unlock()
	late := s.Exec(hv.echo, func(err error) tea.Msg { return handoverDone{err} })

	// The model is in Update when the key is removed: the program
	// cannot stop.
	tm.press("%")
	select {
	case <-hv.blocked:
	case <-time.After(waitTime):
		t.Fatal("the model has not been given the key")
	}
	setKeys(t, h.list)
	if done, ok := late().(handoverDone); !ok || done.err != ErrSessionEnded {
		t.Errorf("the command of a session that is being ended returned %#v, want done's message for ErrSessionEnded", done)
	}
	close(hv.release)
	if status := tm.exit(); status != 1 {
		t.Errorf("exit status %d, want 1", status)
	}
}

// TestCommandPanics has a command panic: the session is closed as it is
// for any panic of its program's, and the server goes on.
func TestCommandPanics(t *testing.T) {
	hv := newHandover(func(context.Context, Terminal) error { panic("test command: panic") })
	h, tm := startHandover(t, hv)
	tm.press("!")
	if status := tm.exit(); status != 1 {
		t.Errorf("exit status %d, want 1", status)
	}
	if out := tm.stdout.String(); !strings.HasSuffix(out, MsgCrashed+"\r\n") {
		t.Errorf("the client was not told %q at the end; the session's last bytes are %q", MsgCrashed, out[max(0, len(out)-100):])
	}
	h.log.waitFor(t, "of the panic", func(r logRecord) bool { return r.Level.String() == "ERROR" && strings.Contains(r.Msg, "panicked") })
	eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })

	// The terminal of another session is handed over as ever.
	hv.note(func() { hv.run = hv.echo })
	tm = openTerm(t, h.dial(h.key("bob", false), "u"))
	tm.waitFor("LASTLINE")
	tm.press("!")
	hv.wait(t, "the command has begun", func() bool { return hv.began == 2 })
	tm.press("A" + endMark)
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
}

// TestHandoverWaitsForAReaderThatIsHeldUp asks for the hand-over when
// the program's reader has keys that the program does not take: that
// reader cannot be told its end yet, and the hand-over waits for it
// beyond drainWait, since a reader that has not been told its end would
// go on reading next to the one Bubble Tea starts after the command.
func TestHandoverWaitsForAReaderThatIsHeldUp(t *testing.T) {
	hv := newHandover(nil)
	hv.run = hv.echo
	h, tm := startHandover(t, hv)
	hv.mu.Lock()
	s := hv.session
	hv.mu.Unlock()
	sess := h.onlySession()
	release := sync.OnceFunc(func() { close(hv.release) })
	defer release()

	// Three keys in one SSH write, so in one read of the program's
	// reader: the model is in Update with the first, the read loop waits
	// for the program with the second, and its reader waits for the read
	// loop.
	tm.press("%xy")
	select {
	case <-hv.blocked:
	case <-time.After(waitTime):
		t.Fatal("the model has not been given the key")
	}

	// The command of a hand-over, run as the program would run it.
	asked := make(chan tea.Msg, 1)
	go func() { asked <- s.Exec(hv.command, func(err error) tea.Msg { return handoverDone{err} })() }()
	select {
	case msg := <-asked:
		t.Fatalf("the hand-over went on (%T) with the program's reader not told its end", msg)
	case <-time.After(2 * drainWait):
	}

	// The program takes its keys, the reader is told, the hand-over goes
	// on: and after it one reader reads, in order.
	release()
	var msg tea.Msg
	select {
	case msg = <-asked:
	case <-time.After(longWait):
		t.Fatal("the hand-over does not go on when the program's reader could be told its end")
	}
	sess.mu.Lock()
	p := sess.program
	sess.mu.Unlock()
	p.Send(msg)
	hv.wait(t, "the command has begun", func() bool { return hv.began == 1 })
	tm.press("AB" + endMark)
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	typed := strings.Repeat("0123456789", 20)
	for i := range len(typed) {
		tm.press(typed[i : i+1])
	}
	hv.wait(t, "the model has every key", func() bool { return len(hv.keys) >= len("%xy")+len(typed) })
	hv.mu.Lock()
	defer hv.mu.Unlock()
	if got := strings.Join(hv.keys, ""); got != "%xy"+typed || string(hv.cmd) != "AB"+endMark || hv.marks != 0 {
		t.Errorf("the model was given %q and %d mode reports, the command %q", got, hv.marks, hv.cmd)
	}
}
