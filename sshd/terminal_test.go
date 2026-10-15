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
	"golang.org/x/crypto/ssh"
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
	told    []error         // what the model's done was told, when it was told
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
	hv := m.hv
	return m.s.Exec(hv.command, func(err error) tea.Msg {
		hv.note(func() { hv.told = append(hv.told, err) })
		return handoverDone{err}
	})
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
	command, drained, told := in.give(0)
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
	if second, _, _ := in.give(0); second != nil {
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
	command, _, _ = in.give(0)
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

	// The end of the channel is every reader's end, and the pump's. The
	// program's reader is given a read of no bytes before it.
	_ = client.Close()
	wantRead(t, "the program before the channel's end", readOnce(program), "", nil)
	wantRead(t, "the program at the channel's end", readOnce(program), "", io.EOF)
	select {
	case <-pumped:
	case <-time.After(waitTime):
		t.Fatal("the pump has not ended with the channel")
	}
	// No reader is left to tell: a hand-over does not wait for one.
	command, drained, told = in.give(0)
	if command == nil || !isClosed(drained) || !isClosed(told) {
		t.Error("a hand-over after the program's reader has ended waits for that reader")
	}
	wantRead(t, "the command at the channel's end", readOnce(command), "", io.EOF)
}

// TestProgramsReaderIsToldItsEndAfterAReadOfNoBytes ends the program's
// reading, as a session's end does: a reader that waits is given a read
// of no bytes, and its end with the read after that, which is when the
// end counts as told. No byte of the client's is the reader's from then
// on, a later reader of the program is told the end at once, and no
// command is given the terminal.
func TestProgramsReaderIsToldItsEndAfterAReadOfNoBytes(t *testing.T) {
	channel, client := io.Pipe()
	in := newInput()
	go in.pump(channel)
	defer func() { _ = client.Close() }()
	program := programInput{in}

	waiting := readOnce(program)
	told := in.endProgram()
	wantRead(t, "the reader that waited", waiting, "", nil)
	if isClosed(told) {
		t.Fatal("the end counts as told with the read of no bytes: the library may still have events of the reads before it")
	}
	if _, err := io.WriteString(client, "late"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the bytes are in", func() bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return len(in.buf) == 4
	})
	wantRead(t, "the reader's next read", readOnce(program), "", io.EOF)
	if !isClosed(told) {
		t.Error("the reader was told its end, and the end does not count as told")
	}
	if again := in.endProgram(); again != told {
		t.Error("a second end of the program's reading has another told")
	}
	wantRead(t, "a later reader of the program", readOnce(program), "", io.EOF)
	if command, _, _ := in.give(0); command != nil {
		t.Error("a command was given the terminal after the program's reading had ended")
	}

	// While a command has the terminal no reader of the program is to
	// come for its end: it counts as told at once. The reader that
	// Bubble Tea starts after the turn is given the same two reads.
	in = newInput()
	command, _, _ := in.give(0)
	if command == nil {
		t.Fatal("no command was given the terminal")
	}
	wantRead(t, "the program's reader at the hand-over", readOnce(programInput{in}), drainMark, nil)
	wantRead(t, "the program's reader after the mark", readOnce(programInput{in}), "", io.EOF)
	if told := in.endProgram(); !isClosed(told) {
		t.Error("the end of the program's reading waits for a reader while a command has the terminal")
	}
	in.takeBack(command)
	wantRead(t, "the reader after the turn", readOnce(programInput{in}), "", nil)
	wantRead(t, "the reader after the turn, again", readOnce(programInput{in}), "", io.EOF)
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
	wantRead(t, "the program's reader before the end of a closed input", waiting, "", nil)
	wantRead(t, "the program's reader of a closed input", readOnce(programInput{in}), "", io.EOF)
	_ = client.Close()
	select {
	case <-pumped:
	case <-time.After(waitTime):
		t.Fatal("the pump has not ended")
	}

	// What waits when the input is closed is nobody's.
	channel, client = io.Pipe()
	in = newInput()
	pumped = make(chan struct{})
	go func() {
		in.pump(channel)
		close(pumped)
	}()
	if _, err := io.WriteString(client, "late"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the bytes are in", func() bool {
		in.mu.Lock()
		defer in.mu.Unlock()
		return len(in.buf) == 4
	})
	in.close()
	wantRead(t, "the program's reader before the end of an input closed with bytes waiting", readOnce(programInput{in}), "", nil)
	wantRead(t, "the program's reader of an input closed with bytes waiting", readOnce(programInput{in}), "", io.EOF)
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
	tm := openTerm(t, h.dial(h.key(admin("ann")), "u"))
	tm.waitFor("LASTLINE")
	sess := h.onlySession()

	// Pastes, so that megabytes are few messages for the model; each of
	// less than pasteLimit, beyond which a paste is cut.
	const size, pastes = 16 << 20, 128
	body := bytes.Repeat([]byte("0123456789abcdef"), size/pastes/16)
	var payload []byte
	for range pastes {
		payload = append(append(append(payload, pasteStart...), body...), pasteEnd...)
	}

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
	hv.wait(t, "the model has the pastes", func() bool { return len(hv.pastes) == pastes })
	hv.mu.Lock()
	got := strings.Join(hv.pastes, "")
	hv.mu.Unlock()
	if got != strings.Repeat(string(body), pastes) {
		t.Errorf("the model was pasted %d bytes, want the %d that were sent, the same", len(got), len(body)*pastes)
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
	tm := openTerm(t, h.dial(h.key(admin("ann")), "u"))
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
// command gets it, and the other, when it has waited busyWait for the
// terminal and the first still has it, is refused without being called.
func TestOneCommandAtATime(t *testing.T) {
	turns := make(chan held, 2)
	hv := newHandover(hold(turns))
	_, tm := startHandover(t, hv)

	asked := time.Now()
	tm.press("@")
	turn := next(t, turns)
	// The first keeps the terminal until the second has been refused.
	hv.wait(t, "the second command was refused", func() bool { return len(hv.told) == 1 })
	if d := time.Since(asked); d < busyWait {
		t.Errorf("the second command was refused after %v: it did not wait %v for the terminal", d, busyWait)
	}
	turn.finish <- nil
	hv.wait(t, "the model was told of both", func() bool { return len(hv.ended) == 2 })
	hv.mu.Lock()
	began, told := hv.began, append([]error(nil), hv.told...)
	hv.mu.Unlock()
	if began != 1 {
		t.Errorf("%d commands were given the terminal, want 1", began)
	}
	if len(told) != 2 || told[0] != ErrTerminalBusy || told[1] != nil {
		t.Errorf("the model was told %v, want ErrTerminalBusy for the second command and then nil for the one that ran", told)
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

// TestHandoverWaitsForARepair runs the command of a hand-over while
// the repair of the screen has the terminal, which is the one thing that
// can have it when the model's command runs: the command is not refused,
// it waits, and the model's run is called when the repair is over.
//
// A key pressed during the repair waits in the input, and the command it
// makes with it; the command that meets a repair is one that was on its
// way already, so the test runs it itself, as the program would.
func TestHandoverWaitsForARepair(t *testing.T) {
	hv := newHandover(nil)
	hv.run = hv.echo
	// A repair long enough to be met, and shorter than busyWait.
	h, tm := startHandover(t, hv, withServer(func(s *Server) { s.repairTurn = busyWait / 2 }))
	sess := h.onlySession()
	hv.mu.Lock()
	s := hv.session
	hv.mu.Unlock()

	// As the session's timer does when the program has not written.
	go sess.checkScreen(sess.out.n.Load())
	eventually(t, "the repair has the terminal", func() bool {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.cancelCommand != nil
	})
	msg := s.Exec(hv.command, func(err error) tea.Msg { return handoverDone{err} })()
	if done, refused := msg.(handoverDone); refused {
		t.Fatalf("the command that met a repair was told %v", done.err)
	}
	sess.mu.Lock()
	p := sess.program
	sess.mu.Unlock()
	p.Send(msg)
	hv.wait(t, "the model's command has begun", func() bool { return hv.began == 1 })
	tm.press("A" + endMark)
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	hv.mu.Lock()
	defer hv.mu.Unlock()
	if hv.ended[0] != nil || string(hv.cmd) != "A"+endMark {
		t.Errorf("the model was told %v, the command read %q", hv.ended[0], hv.cmd)
	}
	if n := h.repairs(); n < 1 {
		t.Errorf("the screen was repaired %d times, want once", n)
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
			cl := h.dial(h.key(admin("ann")), "u")
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
				// At once, and not after the end's timeout of 2 seconds:
				// the bound is wide for the teardown of a connection on a
				// busy machine, and narrower than that.
				if d := time.Since(ended); d > time.Second {
					t.Errorf("the command's context was done %v after the session was ended, want at once", d)
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

			rec := h.log.waitFor(t, "of the session's end", func(r logRecord) bool { return r.Msg == "session end" })
			// The turn is over, and not only the command.
			if n, err := io.WriteString(run.t.Out, "LATEWRITE"); n != 0 || err != io.ErrClosedPipe {
				t.Errorf("a write of the command after the session's end: %d, %v; want 0, io.ErrClosedPipe", n, err)
			}
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
// the session's end, by kosh or by a client that closes its channel and
// keeps its connection, is not held up by it for longer than the end's
// timeout, after which the connection is closed and the log says why.
// The session's goroutines are there until the command returns, as they
// are for a program that does not return.
func TestCommandThatDoesNotReturn(t *testing.T) {
	endings := []struct {
		name   string
		end    func(t *testing.T, h *harness, tm *term)
		reason string
	}{
		{name: "the key is removed", reason: MsgKeyRemoved,
			end: func(t *testing.T, h *harness, _ *term) { setKeys(t, h.list) }},
		{name: "the client closes the channel", reason: reasonChannelClosed,
			end: func(_ *testing.T, _ *harness, tm *term) { _ = tm.sess.Close() }},
	}
	for _, e := range endings {
		t.Run(e.name, func(t *testing.T) {
			stuck := make(chan struct{})
			unstick := sync.OnceFunc(func() { close(stuck) })
			defer unstick()
			runs := make(chan held, 1)
			hv := newHandover(func(ctx context.Context, tt Terminal) error {
				runs <- held{ctx: ctx, t: tt}
				<-stuck
				return nil
			})
			h := handoverHarness(t, hv, withServer(func(s *Server) { s.endTimeout = 200 * time.Millisecond }))
			cl := h.dial(h.key(admin("ann")), "u")
			tm := openTerm(t, cl)
			tm.waitFor("LASTLINE")
			tm.press("!")
			var run held
			select {
			case run = <-runs:
			case <-time.After(longWait):
				t.Fatal("the command was not given the terminal")
			}

			e.end(t, h, tm)
			select {
			case <-run.ctx.Done():
			case <-time.After(longWait):
				t.Fatal("the command's context is not done")
			}
			// The command still has its turn, and nothing of the terminal.
			wantRead(t, "the command's Read after the session was ended", readOnce(run.t.In), "", io.EOF)
			eventually(t, "the command's writes are refused", func() bool {
				_, err := run.t.Out.Write(nil)
				return err == io.ErrClosedPipe
			})
			rec := h.log.waitFor(t, "of the session that did not end", func(r logRecord) bool {
				return strings.HasPrefix(r.Msg, "session did not end in time")
			})
			if !strings.Contains(rec.Msg, "did not return") || rec.Level.String() != "WARN" || rec.Attrs["key"] != admin("ann") || rec.Attrs["reason"] != e.reason {
				t.Errorf("the line %v does not say that the command did not return, or whose session it was and why it was ended", rec)
			}
			// The connection is closed, and the client's session with it.
			closed := make(chan error, 1)
			go func() { closed <- cl.Wait() }()
			select {
			case <-closed:
			case <-time.After(longWait):
				t.Fatal("the connection of the session that did not end is open")
			}
			select {
			case <-tm.done:
			case <-time.After(longWait):
				t.Fatal("the client's session has not ended")
			}

			// When the command does return, nothing of the session is left.
			unstick()
			h.log.waitFor(t, "of the session's end", func(r logRecord) bool { return r.Msg == "session end" && r.Attrs["reason"] == e.reason })
			eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
		})
	}
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
	sess := h.onlySession()
	tm.press("!")
	if status := tm.exit(); status != 1 {
		t.Errorf("exit status %d, want 1", status)
	}
	if out := tm.stdout.String(); !strings.HasSuffix(out, MsgCrashed+"\r\n") {
		t.Errorf("the client was not told %q at the end; the session's last bytes are %q", MsgCrashed, out[max(0, len(out)-100):])
	}
	h.log.waitFor(t, "of the panic", func(r logRecord) bool { return r.Level.String() == "ERROR" && strings.Contains(r.Msg, "panicked") })
	eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
	// The program has returned, and its end does not wait for a reader
	// that nothing reads for any more, to say then that it was left.
	eventually(t, "the end of the program's reading is over", sess.wound.Load)
	h.noReaderGivenUp(t)

	// The terminal of another session is handed over as ever.
	hv.note(func() { hv.run = hv.echo })
	tm = openTerm(t, h.dial(h.key(admin("bob")), "u"))
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

// repairs returns how many times the log says that a session's screen
// was repaired.
func (h *harness) repairs() int {
	n := 0
	for _, r := range h.log.records() {
		if r.Msg == "the program's screen was repaired after a command" {
			if r.Level.String() != "WARN" || r.Attrs["key"] == "" {
				h.t.Errorf("the line %v is not a warning that names the session's key", r)
			}
			n++
		}
	}
	return n
}

// TestScreenAfterACommandThatReturnsAtOnce hands the terminal to a
// command that returns without waiting for anything, as a command that
// is refused does: the program draws after every one of them.
//
// Bubble Tea stops its renderer's goroutine when it releases the
// terminal and starts another when it takes the terminal back, and the
// first, when it is slow to go, stops the ticker of the second: after a
// command that returned too soon the program would draw nothing more.
// Two things stand against that. A turn lasts minTurn at least, which
// lets the first goroutine go in time: with it no screen has to be
// repaired. And the session looks, after each turn, whether its program
// writes: without minTurn and on one CPU, where the screen is dead after
// nearly every command, each is repaired.
func TestScreenAfterACommandThatReturnsAtOnce(t *testing.T) {
	settings := []struct {
		name    string
		procs   int
		minTurn time.Duration
		cycles  int
		repairs bool
	}{
		{name: "as it is", minTurn: minTurn, cycles: 100},
		{name: "as it is, GOMAXPROCS=1", procs: 1, minTurn: minTurn, cycles: 100},
		{name: "a turn of no least time, GOMAXPROCS=1", procs: 1, minTurn: 0, cycles: 30, repairs: true},
	}
	for _, s := range settings {
		t.Run(s.name, func(t *testing.T) {
			if s.procs != 0 {
				before := runtime.GOMAXPROCS(s.procs)
				defer runtime.GOMAXPROCS(before)
			}
			hv := newHandover(func(context.Context, Terminal) error { return nil })
			h, tm := startHandover(t, hv, withServer(func(srv *Server) { srv.minTurn = s.minTurn }))
			for i := 1; i <= s.cycles; i++ {
				tm.press("!")
				hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == i })
				tm.waitFor(fmt.Sprintf("done:%d ", i))
			}
			n := h.repairs()
			t.Logf("%d commands that returned at once: the screen was repaired %d times", s.cycles, n)
			if s.repairs && n == 0 {
				t.Error("no screen was repaired: this setting does not show what it is for")
			}
			// A program that is kept from writing for screenCheck on a
			// busy machine gets a repair it did not need: that is not
			// what this looks for.
			if !s.repairs && n > 2 {
				t.Errorf("%d screens had to be repaired, with the turn's least time in place", n)
			}
		})
	}
}

// TestScreenCheck calls what the session's timer calls after a turn,
// with the number of the program's writes as it is: as if the program
// had written nothing since the turn. A session that never handed over
// has no such timer. While a command has the terminal nothing is done,
// and nothing after the session's end; otherwise the terminal is handed
// over once more, to nothing, which starts the library's renderer anew,
// and that is logged.
func TestScreenCheck(t *testing.T) {
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	h, tm := startHandover(t, hv)
	sess := h.onlySession()
	timer := func() *time.Timer {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return sess.screenTimer
	}
	// The program's own leaving of the alternate screen is followed by
	// its showing of the cursor; the reset's is not.
	const release = "\x1b[?1049l\x1b[?25h"
	releases := func() int { return strings.Count(tm.stdout.String(), release) }

	tm.press("x")
	hv.wait(t, "the model has the key", func() bool { return len(hv.keys) == 1 })
	if timer() != nil {
		t.Error("a session that has not handed its terminal over has a timer for its screen")
	}

	// While a command has the terminal.
	tm.press("!")
	turn := next(t, turns)
	sess.checkScreen(sess.out.n.Load())
	if n := h.repairs(); n != 0 {
		t.Errorf("the screen was repaired %d times while a command had the terminal", n)
	}
	// From here on a repair that the session's own timer makes, for a
	// program kept from writing on a busy machine, is not a failure: two
	// of them at most.
	const needless = 2
	turn.finish <- nil
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	tm.waitFor("done:1 ")
	if timer() == nil {
		t.Error("no timer looks at the screen after a turn")
	}

	// A program that has written nothing since: one more hand-over, to
	// nothing, and the screen is drawn after it.
	before, repaired := releases(), h.repairs()
	sess.checkScreen(sess.out.n.Load())
	if n := h.repairs(); n != repaired+1 {
		t.Fatalf("the screen was repaired %d times by the check, want once", n-repaired)
	}
	eventually(t, "the program has released the terminal once more and drawn again", func() bool {
		out := tm.stdout.String()
		return releases() > before && strings.LastIndex(out, "LASTLINE") > strings.LastIndex(out, "\x1b[?1049l")
	})
	hv.mu.Lock()
	began, ended := hv.began, len(hv.ended)
	hv.mu.Unlock()
	if began != 1 || ended != 1 {
		t.Errorf("the repair was the model's business: %d commands began, the model was told of %d", began, ended)
	}
	// The program has written: the timer of the repair's own turn finds
	// nothing to do. Twice its time, for it to have fired.
	time.Sleep(2 * screenCheck)
	if n := h.repairs(); n > 1+needless {
		t.Errorf("the screen was repaired %d times, want once", n)
	}
	tm.press("y")
	hv.wait(t, "the model has the key typed after the repair", func() bool { return strings.HasSuffix(strings.Join(hv.keys, ""), "y") })

	// After the session's end.
	tm.press("q")
	if status := tm.exit(); status != 0 {
		t.Fatalf("exit status %d", status)
	}
	eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
	h.log.waitFor(t, "of the session's end", func(r logRecord) bool { return r.Msg == "session end" })
	atEnd := h.repairs()
	sess.checkScreen(sess.out.n.Load())
	if n := h.repairs(); n != atEnd {
		t.Errorf("the screen of a session that has ended was repaired")
	}
	t.Logf("the screen was repaired %d times, once by this test", atEnd)
}

// unread is a session of a client that does not read what the session
// writes, until it is told to.
type unread struct {
	cl    *ssh.Client
	sess  *ssh.Session
	stdin io.Writer
	// stdout is what the client would read.
	stdout io.Reader
	// conn counts what arrives at the client's connection.
	conn *countingConn
}

// openUnread opens a session with a terminal for key, whose output
// nobody reads.
func openUnread(t *testing.T, h *harness, key ssh.Signer) *unread {
	t.Helper()
	u := &unread{conn: &countingConn{Conn: h.dialRaw()}}
	var err error
	if u.cl, err = connect(u.conn, h.addr, clientConfig("kosh", ssh.PublicKeys(key))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = u.cl.Close() })
	if u.sess, err = u.cl.NewSession(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = u.sess.Close() })
	if u.stdout, err = u.sess.StdoutPipe(); err != nil {
		t.Fatal(err)
	}
	if u.stdin, err = u.sess.StdinPipe(); err != nil {
		t.Fatal(err)
	}
	if err := u.sess.RequestPty("xterm-256color", 24, 80, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	if err := u.sess.Shell(); err != nil {
		t.Fatal(err)
	}
	return u
}

// stalled waits until the session's output has stopped: the channel's
// window of 2 MB is full, and nothing more arrives for 200 ms.
func (u *unread) stalled(t *testing.T) {
	t.Helper()
	last, since := u.conn.read.Load(), time.Now()
	eventuallyWithin(t, longWait, "the session's output to stall", func() bool {
		if n := u.conn.read.Load(); n != last {
			last, since = n, time.Now()
		}
		return last > 2<<20 && time.Since(since) > 200*time.Millisecond
	})
}

// TestSessionEndsWhileTheClientDoesNotRead removes the key of a session
// whose command is in a write that the client does not take: a command
// that returns on its context and leaves its write to a goroutine, and
// one that waits for its write. The command's context is done and its
// input ends at once; its write cannot be ended before the connection
// is, which the end's timer does: the client gets no message, and the
// log says that it was the write that was waited for.
func TestSessionEndsWhileTheClientDoesNotRead(t *testing.T) {
	const endTimeout = 200 * time.Millisecond
	type writing struct {
		held
		wrote <-chan error
	}
	for _, waits := range []bool{false, true} {
		name := "a command that returns on its context"
		if waits {
			name = "a command that waits for its write"
		}
		t.Run(name, func(t *testing.T) {
			runs := make(chan writing, 1)
			hv := newHandover(func(ctx context.Context, tt Terminal) error {
				wrote := make(chan error, 2)
				write := func() error {
					_, err := tt.Out.Write(make([]byte, 8<<20))
					wrote <- err
					return err
				}
				runs <- writing{held{ctx: ctx, t: tt}, wrote}
				if waits {
					return write()
				}
				go write()
				<-ctx.Done()
				return ctx.Err()
			})
			h := handoverHarness(t, hv, withServer(func(s *Server) { s.endTimeout = endTimeout }))
			u := openUnread(t, h, h.key(admin("ann")))
			if _, err := u.stdin.Write([]byte("!")); err != nil {
				t.Fatal(err)
			}
			var run writing
			select {
			case run = <-runs:
			case <-time.After(longWait):
				t.Fatal("the command was not given the terminal")
			}
			u.stalled(t)
			select {
			case err := <-run.wrote:
				t.Fatalf("the command's write of 8 MB returned (%v) though the client does not read", err)
			default:
			}

			ended := time.Now()
			setKeys(t, h.list)
			select {
			case <-run.ctx.Done():
				if d := time.Since(ended); d > time.Second {
					t.Errorf("the command's context was done %v after the key was removed", d)
				}
			case <-time.After(longWait):
				t.Fatal("the command's context is not done")
			}
			wantRead(t, "the command's Read after the session was ended", readOnce(run.t.In), "", io.EOF)
			select {
			case err := <-run.wrote:
				if err == nil {
					t.Error("the write that the client did not take returned without an error")
				}
				if d := time.Since(ended); d > endTimeout+2*time.Second {
					t.Errorf("the write that the client did not take returned %v after the key was removed, want within the end's timeout of %v and a margin", d, endTimeout)
				}
			case <-time.After(longWait):
				t.Fatal("the write that the client did not take has not returned")
			}
			closed := make(chan error, 1)
			go func() { closed <- u.cl.Wait() }()
			select {
			case <-closed:
			case <-time.After(longWait):
				t.Fatal("the connection of the session that could not be told is open")
			}

			rec := h.log.waitFor(t, "of the session that did not end", func(r logRecord) bool {
				return strings.HasPrefix(r.Msg, "session did not end in time")
			})
			if !strings.Contains(rec.Msg, "the client did not take what its command wrote") || rec.Attrs["key"] != admin("ann") || rec.Attrs["reason"] != MsgKeyRemoved {
				t.Errorf("the line %v does not say that a write of the command's was waited for, or whose session it was and why it was ended", rec)
			}
			h.log.waitFor(t, "of the session's end", func(r logRecord) bool { return r.Msg == "session end" && r.Attrs["reason"] == MsgKeyRemoved })
			eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
		})
	}
}

// TestKeyTypedWhileATurnEnds types a key when a command has returned and
// its turn is not over, because a write of the command's is still under
// way, and a goroutine of the command is still in In.Read: that reader
// was told the end when the command returned, and the key is the
// program's.
func TestKeyTypedWhileATurnEnds(t *testing.T) {
	type running struct {
		read  <-chan readResult
		wrote <-chan error
	}
	runs := make(chan running, 1)
	finish := make(chan struct{})
	hv := newHandover(func(ctx context.Context, tt Terminal) error {
		wrote := make(chan error, 1)
		go func() {
			_, err := tt.Out.Write(make([]byte, 8<<20))
			wrote <- err
		}()
		runs <- running{readOnce(tt.In), wrote}
		select {
		case <-finish:
		case <-ctx.Done():
		}
		return nil
	})
	h := handoverHarness(t, hv)
	u := openUnread(t, h, h.key(admin("ann")))
	if _, err := u.stdin.Write([]byte("!")); err != nil {
		t.Fatal(err)
	}
	var run running
	select {
	case run = <-runs:
	case <-time.After(longWait):
		t.Fatal("the command was not given the terminal")
	}
	u.stalled(t)

	// The command returns; its turn waits for its write.
	close(finish)
	wantRead(t, "the command's reader when the command has returned", run.read, "", io.EOF)
	if _, err := u.stdin.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	// The client reads again: the write is taken, the turn ends, and the
	// program has the key.
	go func() { _, _ = io.Copy(io.Discard, u.stdout) }()
	select {
	case err := <-run.wrote:
		if err != nil {
			t.Errorf("the command's write, which the client took in the end: %v", err)
		}
	case <-time.After(longWait):
		t.Fatal("the command's write has not returned though the client reads")
	}
	hv.wait(t, "the model has the key typed while the turn ended", func() bool { return strings.Join(hv.keys, "") == "!x" })
}

// TestGuardLetsGoOfTheModel releases a guard, as the session does when
// its program has returned: the model is held no more, and a call that
// comes late does nothing.
func TestGuardLetsGoOfTheModel(t *testing.T) {
	var stopped atomic.Bool
	m := makeModel(func() tea.Model { return testModel{} }, &stopped, func(string, any, []byte) {
		t.Error("the guard reported a panic")
	})
	g := m.(*guard)
	g.release()
	if g.inner != nil {
		t.Error("a released guard holds its model")
	}
	if cmd := g.Init(); cmd != nil {
		t.Error("Init of a released guard returned a command")
	}
	if next, cmd := g.Update(tea.KeyPressMsg{}); next != tea.Model(g) || cmd != nil {
		t.Error("Update of a released guard did something")
	}
	_ = g.View()
}

// leftModes is what a full-screen program that was killed leaves, and
// more: the alternate screen, mouse reports, application cursor keys, a
// scroll region with origin mode, the line-drawing set, a red
// background.
const leftModes = "\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?1h\x1b[5;10r\x1b[?6h\x1b(0\x1b[41m"

// TestTerminalIsResetAfterACommand has a command leave the terminal in
// modes of its own: the first thing the client receives after the
// command's last byte is the reset, before anything of the program's,
// and the program's frame is then drawn as it would be on a terminal
// nobody had touched.
func TestTerminalIsResetAfterACommand(t *testing.T) {
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	_, tm := startHandover(t, hv)
	tm.press("!")
	turn := next(t, turns)
	if _, err := io.WriteString(turn.t.Out, leftModes+"LASTBYTE"); err != nil {
		t.Fatal(err)
	}
	tm.waitFor("LASTBYTE")
	turn.finish <- nil
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	tm.waitFor("done:1 ")

	out := tm.stdout.String()
	after := out[strings.Index(out, "LASTBYTE")+len("LASTBYTE"):]
	t.Logf("the client received after the command's last byte: %q", after[:min(len(after), len(terminalReset)+60)])
	if !strings.HasPrefix(after, terminalReset) {
		t.Errorf("what the client received after the command's last byte does not begin with the reset")
	}
	if !strings.Contains(strings.TrimPrefix(after, terminalReset), "\x1b[?1049h") {
		t.Error("the program did not take the terminal back after the reset")
	}
	want := strings.Join(handoverFrame(1, 80, 24), "\n")
	eventually(t, "the model's frame fills the screen after the return", func() bool { return screenOf(tm).String() == want })
}

// TestTerminalIsResetWhenTheSessionEndsInACommand removes the key of a
// session whose command has left the terminal in modes of its own: the
// reset arrives before the session's last message.
func TestTerminalIsResetWhenTheSessionEndsInACommand(t *testing.T) {
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	h, tm := startHandover(t, hv)
	tm.press("!")
	turn := next(t, turns)
	if _, err := io.WriteString(turn.t.Out, leftModes+"LASTBYTE"); err != nil {
		t.Fatal(err)
	}
	tm.waitFor("LASTBYTE")
	setKeys(t, h.list)
	if status := tm.exit(); status != 1 {
		t.Errorf("exit status %d, want 1", status)
	}
	out := tm.stdout.String()
	after := out[strings.Index(out, "LASTBYTE")+len("LASTBYTE"):]
	if !strings.HasPrefix(after, terminalReset) {
		t.Errorf("what the client received after the command's last byte does not begin with the reset: %q", after[:min(len(after), 80)])
	}
	if !strings.HasSuffix(after, MsgKeyRemoved+"\r\n") {
		t.Errorf("the client was not told %q at the end: %q", MsgKeyRemoved, after[max(0, len(after)-80):])
	}
	// The message is on the normal screen, in the first column.
	scr := screenOf(tm).String()
	if !strings.Contains("\n"+scr, "\n"+MsgKeyRemoved) {
		t.Errorf("the session's last message is not at the start of a line of the screen:\n%s", scr)
	}
}

// heapNow returns the bytes of the heap that are in use after a
// collection.
func heapNow() int64 {
	// Twice: what a finalizer frees is gone with the second.
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return int64(m.HeapAlloc)
}

// sendAll writes payload to the session in pieces, and returns where
// the result will be and how much has been sent.
func sendAll(tm *term, payload []byte) (<-chan error, *atomic.Int64) {
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
	return wrote, &sent
}

// TestPasteToTheProgramIsBounded sends the program a paste of 16 MiB:
// the model gets one paste, of pasteLimit bytes, properly ended, the keys
// before and after it, and what the server holds meanwhile does not
// grow with what the client sends. A paste within the limit is the
// program's as it was sent.
func TestPasteToTheProgramIsBounded(t *testing.T) {
	hv := newHandover(nil)
	_, tm := startHandover(t, hv)
	const size = 16 << 20
	body := bytes.Repeat([]byte("0123456789abcdef"), size/16)
	payload := []byte("a" + pasteStart)
	payload = append(payload, body...)
	payload = append(payload, pasteEnd+"xyz"...)

	before := heapNow()
	wrote, sent := sendAll(tm, payload)
	var most int64
	sending := true
	for deadline := time.Now().Add(longWait); sending || hv.typed() != "axyz"; {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v: %d of %d bytes are sent, the model has the keys %q", longWait, sent.Load(), len(payload), hv.typed())
		}
		most = max(most, heapNow()-before)
		select {
		case err := <-wrote:
			if err != nil {
				t.Fatalf("sending: %v", err)
			}
			sending = false
		case <-time.After(time.Millisecond):
		}
	}
	hv.mu.Lock()
	pastes := append([]string(nil), hv.pastes...)
	hv.mu.Unlock()
	if len(pastes) != 1 {
		t.Fatalf("the model was given %d pastes, want 1", len(pastes))
	}
	t.Logf("of a paste of %d bytes the model was given %d; the heap grew by %d bytes at most while it was sent", size, len(pastes[0]), most)
	if len(pastes[0]) > pasteLimit || len(pastes[0]) < pasteLimit-len(pasteEnd) || pastes[0] != string(body[:len(pastes[0])]) {
		t.Errorf("the model was given a paste of %d bytes, want the first %d of what was sent", len(pastes[0]), pasteLimit)
	}
	// The library makes of the paste it is given a slice of runes, a
	// string, and an event for every piece on the way: several times the
	// limit for a moment, as measured, and the same however much the
	// client sends. Without the limit this paste took 146 MB.
	if most > 16<<20 {
		t.Errorf("the heap grew by %d bytes while a paste of %d was sent", most, size)
	}

	// A paste of the limit exactly, and what follows it.
	small := strings.Repeat("0123456789abcdef", pasteLimit/16)
	tm.press(pasteStart + small + pasteEnd + "b")
	hv.wait(t, "the model has the second paste and the key after it", func() bool { return len(hv.pastes) == 2 && strings.Join(hv.keys, "") == "axyzb" })
	if hv.pastes[1] != small {
		t.Errorf("a paste of %d bytes reached the model as %d bytes", len(small), len(hv.pastes[1]))
	}
}

// TestPasteThatNeverEnds sends megabytes after the start of a paste and
// no end: the server takes them all off the channel and holds none of
// what is beyond the limit, and the session ends as any other.
func TestPasteThatNeverEnds(t *testing.T) {
	hv := newHandover(nil)
	h, tm := startHandover(t, hv)
	sess := h.onlySession()
	// SSH's window is 2 MiB: when the client has sent the last 3 MiB,
	// the server has taken the 16 MiB before them off the channel, and
	// out of its own buffer.
	const size, more = 16 << 20, 3 << 20
	payload := append([]byte(pasteStart), bytes.Repeat([]byte("0123456789abcdef"), (size+more)/16)...)

	before := heapNow()
	wrote, sent := sendAll(tm, payload)
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatalf("sending: %v", err)
		}
	case <-time.After(longWait):
		t.Fatalf("the client has sent %d of %d bytes of a paste that does not end", sent.Load(), len(payload))
	}
	grown := heapNow() - before
	// What was sent is in both measurements.
	runtime.KeepAlive(payload)
	t.Logf("after %d bytes of a paste that does not end the heap has grown by %d bytes", size, grown)
	if grown > 8<<20 {
		t.Errorf("the heap grew by %d bytes for a paste of %d that does not end", grown, size)
	}

	setKeys(t, h.list)
	if status := tm.exit(); status != 1 {
		t.Errorf("exit status %d, want 1", status)
	}
	if out := tm.stdout.String(); !strings.HasSuffix(out, MsgKeyRemoved+"\r\n") {
		t.Errorf("the client was not told %q at the end", MsgKeyRemoved)
	}
	eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
	sess.in.mu.Lock()
	defer sess.in.mu.Unlock()
	if len(sess.in.buf) != 0 {
		t.Errorf("the input of a session that has ended holds %d bytes", len(sess.in.buf))
	}
}

// TestPasteToACommandIsWhole sends the same 16 MiB, with the marks of a
// paste, while a command has the terminal: every byte is the command's.
func TestPasteToACommandIsWhole(t *testing.T) {
	const size = 16 << 20
	payload := []byte(pasteStart)
	payload = append(payload, bytes.Repeat([]byte("0123456789abcdef"), size/16)...)
	payload = append(payload, pasteEnd...)
	got := make(chan []byte, 1)
	hv := newHandover(func(_ context.Context, tt Terminal) error {
		b := make([]byte, len(payload))
		n, _ := io.ReadFull(tt.In, b)
		got <- b[:n]
		return nil
	})
	_, tm := startHandover(t, hv)
	tm.press("!")
	hv.wait(t, "the command has begun", func() bool { return hv.began == 1 })
	wrote, sent := sendAll(tm, payload)
	select {
	case b := <-got:
		if !bytes.Equal(b, payload) {
			t.Errorf("the command read %d bytes, want the %d that were sent, the same", len(b), len(payload))
		}
	case <-time.After(longWait):
		t.Fatalf("the command has not read what was sent: %d of %d bytes are sent", sent.Load(), len(payload))
	}
	if err := <-wrote; err != nil {
		t.Fatalf("sending: %v", err)
	}
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
}

// scan passes src through a pasteScanner in reads of the sizes given,
// the last of them repeated, into a buffer of room bytes, and returns
// what came out.
func scan(src string, room int, sizes ...int) string {
	var s pasteScanner
	var out []byte
	dst := make([]byte, room)
	rest := []byte(src)
	for i := 0; len(rest) > 0 || len(s.pending) > 0; i++ {
		piece := rest[:min(len(rest), sizes[min(i, len(sizes)-1)])]
		for len(piece) > 0 || len(s.pending) > 0 {
			nd, ns := s.filter(dst, piece)
			out = append(out, dst[:nd]...)
			piece, rest = piece[ns:], rest[ns:]
		}
	}
	return string(out)
}

// TestPasteScanner feeds the scanner what a client may send, in one
// piece and cut at every place, into buffers large and of one byte: what
// comes out is the same however it was cut.
func TestPasteScanner(t *testing.T) {
	big := strings.Repeat("0123456789abcdef", (pasteLimit+4096)/16)
	cases := []struct{ name, in, want string }{
		{"keys", "abc\x1b[A\x1b\x1bOP", "abc\x1b[A\x1b\x1bOP"},
		{"a paste within the limit", "a" + pasteStart + "pasted" + pasteEnd + "b", "a" + pasteStart + "pasted" + pasteEnd + "b"},
		{"an end without a start", "a" + pasteEnd + "b", "a" + pasteEnd + "b"},
		{"what looks like a mark and is none", "\x1b[20x\x1b[200\x1b[201", "\x1b[20x\x1b[200\x1b[201"},
		{"a paste beyond the limit", "a" + pasteStart + big + pasteEnd + "b" + pasteStart + "x" + pasteEnd, "a" + pasteStart + big[:pasteLimit] + pasteEnd + "b" + pasteStart + "x" + pasteEnd},
		{"escapes in what is dropped", pasteStart + big + "\x1b[20\x1b[201\x1b\x1b[200~" + pasteEnd + "c", pasteStart + big[:pasteLimit] + pasteEnd + "c"},
		{"a paste that does not end", pasteStart + big + big, pasteStart + big[:pasteLimit]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := scan(c.in, 4096, len(c.in)); got != c.want {
				t.Fatalf("in one piece: %d bytes came out, want %d", len(got), len(c.want))
			}
			if got := scan(c.in, 1, 7); got != c.want {
				t.Fatalf("into a buffer of one byte: %d bytes came out, want %d", len(got), len(c.want))
			}
			// Cut in two at every place near a mark, and at every place
			// when the input is short.
			for cut := 1; cut < len(c.in); cut++ {
				if len(c.in) > 200 && !strings.Contains(c.in[max(0, cut-8):min(len(c.in), cut+8)], "\x1b") && cut%4099 != 0 {
					continue
				}
				if got := scan(c.in, 4096, cut, len(c.in)); got != c.want {
					t.Fatalf("cut after %d bytes: %d bytes came out, want %d", cut, len(got), len(c.want))
				}
			}
		})
	}
	// The end of a paste whose limit is reached in the middle of the end
	// mark: the mark comes out whole, once.
	for k := 1; k < len(pasteEnd); k++ {
		body := big[:pasteLimit-k]
		in := pasteStart + body + pasteEnd + "z"
		if got := scan(in, 4096, len(in)); got != in {
			t.Errorf("a paste that ends %d bytes before the limit: %d bytes came out, want the %d that went in", k, len(got), len(in))
		}
	}
}

// TestPasteMarkAcrossThePumpsBuffer sends a paste whose end mark lies
// across the place where the session's buffer is full, and across two
// reads of the program: the program is given the bytes as they were
// sent, and the scanner knows the paste has ended.
func TestPasteMarkAcrossThePumpsBuffer(t *testing.T) {
	for split := 1; split < len(pasteEnd); split++ {
		channel, client := io.Pipe()
		in := newInput()
		go in.pump(channel)
		sent := pasteStart + strings.Repeat("x", inputBuffer-len(pasteStart)-split) + pasteEnd + "after"
		go func() { _, _ = io.WriteString(client, sent) }()
		eventually(t, "the buffer is full", func() bool {
			in.mu.Lock()
			defer in.mu.Unlock()
			return len(in.buf) == inputBuffer
		})
		var got []byte
		b := make([]byte, 1000)
		for len(got) < len(sent) {
			n, err := programInput{in}.Read(b)
			if err != nil {
				t.Fatalf("split %d: reading: %v", split, err)
			}
			got = append(got, b[:n]...)
		}
		in.mu.Lock()
		inPaste := in.paste.in
		in.mu.Unlock()
		if string(got) != sent || inPaste {
			t.Errorf("split %d: the program read %d bytes of %d, the same: %v; the scanner is in a paste: %v", split, len(got), len(sent), string(got) == sent, inPaste)
		}
		_ = client.Close()
	}
}

// at returns the screen of an 80 by 24 terminal that was sent what the
// session has written up to the end of the n-th text among its output,
// and the place of the cursor then, column and row from 0.
func at(t *testing.T, tm *term, text string, n int) (scr *screen, x, y int) {
	t.Helper()
	out := tm.stdout.String()
	end := 0
	for range n {
		i := strings.Index(out[end:], text)
		if i < 0 {
			t.Fatalf("the session has not written %q %d times", text, n)
		}
		end += i + len(text)
	}
	scr = feed(80, 24, out[:end])
	scr.mu.Lock()
	defer scr.mu.Unlock()
	return scr, scr.x, scr.y
}

// rowOf returns the row of the screen that is text, or -1.
func rowOf(scr *screen, text string) int {
	for y, line := range scr.lines() {
		if strings.TrimRight(line, " ") == text {
			return y
		}
	}
	return -1
}

// TestResetLeavesTheCursor has a command print lines on the normal
// screen and return: when the reset has been written the cursor is where
// the command left it, below what it printed, and not at the top of the
// screen, where the next thing printed would be written over the old.
func TestResetLeavesTheCursor(t *testing.T) {
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	_, tm := startHandover(t, hv)
	tm.press("!")
	turn := next(t, turns)
	if _, err := io.WriteString(turn.t.Out, "one\r\ntwo\r\nthree\r\nhalf"); err != nil {
		t.Fatal(err)
	}
	tm.waitFor("half")
	_, x0, y0 := at(t, tm, "half", 1)
	turn.finish <- nil
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	tm.waitFor("done:1 ")

	scr, x, y := at(t, tm, terminalReset, 1)
	if x != x0 || y != y0 || y != rowOf(scr, "half") || x != len("half") {
		t.Errorf("after the reset the cursor is in column %d of row %d; the command left it in column %d of row %d, after its last word in row %d", x, y, x0, y0, rowOf(scr, "half"))
	}
	if scr.alt || scr.top != 0 || scr.bot != 23 {
		t.Errorf("after the reset the alternate screen is on: %v; the margins are rows %d to %d", scr.alt, scr.top, scr.bot)
	}
}

// TestResetAfterAFullScreenProgram has a command print on the normal
// screen, then go to the alternate screen, set margins and origin mode,
// and return there, as a full-screen program that was killed: after the
// reset the terminal shows the normal screen with what was printed, the
// margins are the screen's, and the cursor is below what was printed.
func TestResetAfterAFullScreenProgram(t *testing.T) {
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	_, tm := startHandover(t, hv)
	tm.press("!")
	turn := next(t, turns)
	if _, err := io.WriteString(turn.t.Out, "one\r\ntwo\r\n"+"\x1b[?1049h\x1b[5;10r\x1b[?6h\x1b[41mFULLSCREEN\x1b[3;7H"); err != nil {
		t.Fatal(err)
	}
	tm.waitFor("\x1b[3;7H")
	turn.finish <- nil
	hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
	tm.waitFor("done:1 ")

	scr, x, y := at(t, tm, terminalReset, 1)
	if scr.alt || scr.top != 0 || scr.bot != 23 {
		t.Errorf("after the reset the alternate screen is on: %v; the margins are rows %d to %d", scr.alt, scr.top, scr.bot)
	}
	if rowOf(scr, "one") != 0 || rowOf(scr, "two") != 1 || rowOf(scr, "FULLSCREEN") != -1 {
		t.Errorf("after the reset the screen is not the normal one with what was printed there:\n%s", scr)
	}
	if x != 0 || y != 2 {
		t.Errorf("after the reset the cursor is in column %d of row %d, want column 0 of row 2, below what was printed", x, y)
	}
}

// TestTwoShellsInARow has two commands print, one after the other: the
// second's line is below the first's on the normal screen, not on top
// of it.
func TestTwoShellsInARow(t *testing.T) {
	turns := make(chan held, 1)
	hv := newHandover(hold(turns))
	_, tm := startHandover(t, hv)
	for i, word := range []string{"first", "second"} {
		tm.press("!")
		turn := next(t, turns)
		if _, err := io.WriteString(turn.t.Out, word+"\r\n"); err != nil {
			t.Fatal(err)
		}
		tm.waitFor(word + "\r\n")
		if word == "second" {
			scr, _, y := at(t, tm, "second\r\n", 1)
			if rowOf(scr, "first") != 0 || rowOf(scr, "second") != 1 || y != 2 {
				t.Errorf("while the second command has the terminal the cursor is in row %d and the screen is:\n%s", y, scr)
			}
		}
		turn.finish <- nil
		hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == i+1 })
	}
}

// TestLastMessageIsBelowTheTranscript ends a session by removing its
// key, after a command has printed and returned, and while a command
// that has printed has the terminal: the session's last message is on
// the line below what the command printed.
func TestLastMessageIsBelowTheTranscript(t *testing.T) {
	for _, inside := range []bool{false, true} {
		name := "after a command"
		if inside {
			name = "in a command"
		}
		t.Run(name, func(t *testing.T) {
			turns := make(chan held, 1)
			hv := newHandover(hold(turns))
			h, tm := startHandover(t, hv)
			tm.press("!")
			turn := next(t, turns)
			if _, err := io.WriteString(turn.t.Out, "one\r\ntranscript\r\n"); err != nil {
				t.Fatal(err)
			}
			tm.waitFor("transcript\r\n")
			if !inside {
				turn.finish <- nil
				hv.wait(t, "the model knows that the command ended", func() bool { return len(hv.ended) == 1 })
				tm.waitFor("done:1 ")
			}
			setKeys(t, h.list)
			if status := tm.exit(); status != 1 {
				t.Errorf("exit status %d, want 1", status)
			}
			scr := screenOf(tm)
			if rowOf(scr, "transcript") != 1 || rowOf(scr, MsgKeyRemoved) != 2 {
				t.Errorf("the session's last message is not on the line below what the command printed:\n%s", scr)
			}
		})
	}
}

// floods are beginnings of input that the library gathers, and makes no
// event of, for as long as the client goes on sending: pastes spelled as
// the scanner does not know them and the library does, and sequences and
// strings that do not end. more is what is sent after the beginning,
// again and again. cut is set for one that the session does not end
// for: the scanner takes what follows for a paste, and drops what is
// beyond a paste's limit.
var floods = []struct {
	name, start, more string
	cut               bool
}{
	{"a paste with a zero before its number", "\x1b[0200~", "x", false},
	{"a paste with the 8-bit introducer", "\x9b200~", "x", false},
	{"a paste with a second parameter", "\x1b[200;1~", "x", false},
	{"a control sequence that does not end", "\x1b[", "1;", false},
	{"a control sequence that does not end, 8-bit", "\x9b", "1;", false},
	{"an operating system command that does not end", "\x1b]", "x", false},
	{"an operating system command, 8-bit", "\x9d", "x", false},
	{"a device control string that does not end", "\x1bP", "x", false},
	{"a device control string, 8-bit", "\x90", "x", false},
	{"an application program command that does not end", "\x1b_", "x", false},
	{"an application program command, 8-bit", "\x9f", "x", false},
	{"a start of string that does not end", "\x1bX", "x", false},
	{"a privacy message that does not end", "\x1b^", "x", false},
	{"a string in a paste the scanner does not know", "\x1b[0200~" + "pasted" + "\x1b]", "x", false},
	{"a paste in a string", "\x1b]" + "0;title" + pasteStart, "x", true},
	{"a control sequence after keys and a paste", "keys" + pasteStart + "pasted" + pasteEnd + "\x1b[", "1;", false},
}

// sendFlood sends start, one byte to a write so that it is cut at every
// place, and then size bytes of more, until the session takes no more.
func sendFlood(tm *term, start, more string, size int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range len(start) {
			if _, err := io.WriteString(tm.stdin, start[i:i+1]); err != nil {
				return
			}
		}
		piece := []byte(strings.Repeat(more, (16<<10)/len(more)))
		for sent := 0; sent < size; sent += len(piece) {
			if _, err := tm.stdin.Write(piece); err != nil {
				return
			}
		}
	}()
	return done
}

// floodLines returns how many times the log says that a session was
// ended for input that the library made no event of.
func (h *harness) floodLines() int {
	n := 0
	for _, r := range h.log.records() {
		if strings.HasPrefix(r.Msg, "session's input") {
			if r.Level.String() != "WARN" || r.Attrs["key"] == "" || r.Attrs["fingerprint"] == "" {
				h.t.Errorf("the line %v is not a warning that names the session's key and its fingerprint", r)
			}
			n++
		}
	}
	return n
}

// floodedSessionBound bounds one session sent a flood, by the whole
// process heap's growth. It is loose: the first flooded session pays
// one-time costs (the library's parser tables and buffers, grown to
// hold what it gathers before an event, and a copy of that buffer it
// makes each read), and the number is the growth of the whole heap, not
// of the session alone. What a flood makes a session hold does not grow
// with the session: TestTwentyFloodedSessions, with floodedResidueBound,
// is the measure of that, and is where a per-session leak would show.
const floodedSessionBound = 16 << 20

// floodedResidueBound is how much the heap may hold after a flooded
// session has ended and been forgotten: nothing of it should be left,
// and twenty do not leave more than one. What the library gathers is
// eventlessLimit bytes outside a paste, or pasteLimit in one, which it
// turns into events, at most one per byte at some tens of bytes each,
// for the moment before the session ends: a few megabytes, freed with
// the session.
const floodedResidueBound = 8 << 20

// TestInputWithoutAnEventEndsTheSession sends megabytes of each flood:
// the session is ended, with a message of its own and a line in the log,
// when it has taken eventlessLimit bytes of it; what it holds until then
// is bounded, and nothing of it is left.
func TestInputWithoutAnEventEndsTheSession(t *testing.T) {
	for _, f := range floods {
		t.Run(f.name, func(t *testing.T) {
			hv := newHandover(nil)
			h, tm := startHandover(t, hv)
			before := heapNow()
			sent := sendFlood(tm, f.start, f.more, 16<<20)
			var most int64
			for deadline := time.Now().Add(longWait); !tm.ended(); {
				if time.Now().After(deadline) {
					t.Fatalf("waited %v: the session that is sent %q and no end of it has not ended; the heap has grown by %d bytes", longWait, f.start, heapNow()-before)
				}
				most = max(most, heapNow()-before)
				if f.cut {
					select {
					case <-sent:
						// All of it was taken, and the session goes on,
						// holding no more than a paste's limit of it. The
						// live heap once it has settled, not the peak
						// while the stream and the client's own buffers
						// were in flight, is what the server holds.
						left := heapNow() - before
						t.Logf("the session took the flood and cut it; the heap then held %d bytes more (the peak while it streamed was %d)", left, most)
						if left > floodedSessionBound {
							t.Errorf("the session that cut the flood holds %d bytes, want at most %d", left, floodedSessionBound)
						}
						if n := h.floodLines(); n != 0 {
							t.Errorf("the session was ended for input that it cut")
						}
						setKeys(t, h.list)
						if status := tm.exit(); status != 1 {
							t.Errorf("exit status %d, want 1", status)
						}
						eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
						return
					default:
					}
				}
			}
			if status := tm.exit(); status != 1 {
				t.Errorf("exit status %d, want 1", status)
			}
			if out := tm.stdout.String(); !strings.HasSuffix(out, MsgInputFlood+"\r\n") {
				t.Errorf("the client was not told %q at the end; the session's last bytes are %q", MsgInputFlood, out[max(0, len(out)-100):])
			}
			if n := h.floodLines(); n != 1 {
				t.Errorf("the log has %d lines of a session ended for its input, want 1", n)
			}
			eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
			left := heapNow() - before
			t.Logf("the heap grew by %d bytes at most while the session was flooded, and by %d when it had ended", most, left)
			if most > floodedSessionBound {
				t.Errorf("the heap grew by %d bytes while the session was flooded, want at most %d", most, floodedSessionBound)
			}
		})
	}
}

// TestTwentyFloodedSessions floods twenty sessions, one after the other,
// on one connection: each is ended, and when all have ended the heap has
// not grown by what one of them may hold, and no goroutine is left (the
// harness looks, also for the library's reader, which is left for good
// when the program ends before it has taken what the reader made).
func TestTwentyFloodedSessions(t *testing.T) {
	hv := newHandover(nil)
	h := handoverHarness(t, hv)
	cl := h.dial(h.key(admin("ann")), "u")
	before := heapNow()
	var ending []int
	for i, f := range floods {
		if !f.cut {
			ending = append(ending, i)
		}
	}
	for i := range 20 {
		f := floods[ending[i%len(ending)]]
		tm := openTerm(t, cl)
		tm.waitFor("LASTLINE")
		sendFlood(tm, f.start, f.more, 16<<20)
		select {
		case <-tm.done:
		case <-time.After(longWait):
			t.Fatalf("session %d, sent %s, has not ended", i, f.name)
		}
		_ = tm.sess.Close()
	}
	eventually(t, "the sessions are gone", func() bool { return h.srv.Sessions() == 0 })
	if n := h.floodLines(); n != 20 {
		t.Errorf("the log has %d lines of a session ended for its input, want 20", n)
	}
	left := heapNow() - before
	t.Logf("after twenty flooded sessions the heap has grown by %d bytes", left)
	if left > floodedResidueBound {
		t.Errorf("after twenty flooded sessions the heap has grown by %d bytes, more than one may hold (%d)", left, floodedResidueBound)
	}
}

// TestInputOfATerminalIsNotAFlood sends what terminals do send in
// quantity, more of each than eventlessLimit: a key held down, mouse
// reports, and the replies to the program's questions at its start. The
// library makes events of all of it, and the session goes on.
func TestInputOfATerminalIsNotAFlood(t *testing.T) {
	hv := newHandover(nil)
	h, tm := startHandover(t, hv)
	const mouse = "\x1b[<35;10;10M"
	replies := "\x1b[?2026;2$y\x1b[?2027;0$y\x1b[?1u\x1b[>1;10;0c\x1b]11;rgb:0000/0000/0000\x1b\\"
	sent := []string{
		strings.Repeat(replies, 2),
		strings.Repeat("j", eventlessLimit+eventlessLimit/4),
		strings.Repeat(mouse, (eventlessLimit+eventlessLimit/4)/len(mouse)),
		strings.Repeat(replies, eventlessLimit/len(replies)+1),
		"k",
	}
	wrote, _ := sendAll(tm, []byte(strings.Join(sent, "")))
	keys := eventlessLimit + eventlessLimit/4 + 1
	hv.wait(t, "the model has every key", func() bool { return len(hv.keys) >= keys })
	if err := <-wrote; err != nil {
		t.Fatalf("sending: %v", err)
	}
	hv.mu.Lock()
	n, last := len(hv.keys), hv.keys[len(hv.keys)-1]
	hv.mu.Unlock()
	if n != keys || last != "k" {
		t.Errorf("the model was given %d keys, the last of them %q; want %d and k", n, last, keys)
	}
	if tm.ended() || h.floodLines() != 0 {
		t.Errorf("the session was ended for the input of a terminal")
	}
}

// TestControlSequenceOfFortyParameters sends complete control sequences
// with more parameters than the library's parser has room for, each
// followed by an ordinary key: the library does not panic (it would be
// the end of the test's process, as it would be of kosh), the session
// goes on, and the key arrives at the model as that key, once. So the
// guard, which drops the separators beyond maxSeparators, neither
// swallows what follows a sequence nor loses its place in the input.
func TestControlSequenceOfFortyParameters(t *testing.T) {
	params := func(sep string) string { return strings.Repeat("1"+sep, 39) + "1" }
	cases := []struct {
		name string
		seq  string
		// split sends the sequence one write to each separator.
		split bool
	}{
		{name: "a control sequence", seq: "\x1b[" + params(";") + "m"},
		{name: "split at each separator", seq: "\x1b[" + params(";") + "m", split: true},
		{name: "with colons", seq: "\x1b[" + params(":") + "m"},
		{name: "with colons, split at each", seq: "\x1b[" + params(":") + "m", split: true},
		{name: "with the 8-bit introducer", seq: "\x9b" + params(";") + "m"},
		{name: "with the 8-bit introducer, split at each separator", seq: "\x9b" + params(";") + "m", split: true},
		{name: "a device control string", seq: "\x1bP" + params(";") + "pdata\x1b\\"},
		{name: "a device control string, split at each separator", seq: "\x1bP" + params(";") + "pdata\x1b\\", split: true},
		{name: "a device control string with the 8-bit introducer", seq: "\x90" + params(";") + "pdata\x1b\\"},
	}
	hv := newHandover(nil)
	h, tm := startHandover(t, hv)
	for i, c := range cases {
		key := string(rune('a' + i))
		if c.split {
			rest := c.seq
			for len(rest) > 0 {
				n := strings.IndexAny(rest, ";:") + 1
				if n == 0 {
					n = len(rest)
				}
				tm.press(rest[:n])
				rest = rest[n:]
			}
			tm.press(key)
		} else {
			tm.press(c.seq + key)
		}
		hv.wait(t, c.name+": the model has the key after the sequence", func() bool {
			return len(hv.keys) > 0 && hv.keys[len(hv.keys)-1] == key
		})
		hv.mu.Lock()
		keys := strings.Join(hv.keys, "")
		hv.mu.Unlock()
		if want := "abcdefghi"[:i+1]; keys != want {
			t.Fatalf("%s: the model has the keys %q, want %q: each key after its sequence, once, and nothing of the sequence", c.name, keys, want)
		}
	}
	if tm.ended() || h.floodLines() != 0 {
		t.Error("the session was ended for control sequences of forty parameters")
	}
}

// slowLoop makes the program's event loop busy for d with each event of
// its input, as the race detector does on a loaded machine: a session
// may then be ended while the library's reader has events in hand. The
// loop is busy and does not sleep: a sleep of less than a millisecond
// lasts a millisecond on Linux.
func slowLoop(d time.Duration) option {
	return withServer(func(s *Server) {
		s.onInput = func() {
			for began := time.Now(); time.Since(began) < d; {
			}
		}
	})
}

// openStates are what the library may hold unfinished when a session is
// ended: a prefix with nothing closing it. mid-paste in several
// spellings, mid-CSI, every string kind, half a UTF-8 character.
var openStates = []struct{ name, prefix string }{
	{"mid-paste", "\x1b[200~pasted text that never ends"},
	{"mid-paste, a zero in the mark", "\x1b[0200~pasted"},
	{"mid-paste, the 8-bit introducer", "\x9b200~pasted"},
	{"mid-CSI", "\x1b[1;2;3;4;5"},
	{"mid-OSC", "\x1b]0;a title with no terminator"},
	{"mid-OSC, 8-bit", "\x9d0;a title"},
	{"mid-DCS", "\x1bP1;2qsixel-ish data"},
	{"mid-APC", "\x1b_application program command"},
	{"mid-SOS", "\x1bXstart of string"},
	{"mid-PM", "\x1b^privacy message"},
	{"half a UTF-8 character", "\xc3"},
	{"an escape pending", "\x1b"},
}

// TestSessionEndsMidSequence ends a session, by removing its key, while
// the library holds each open state: with a slow event loop and one CPU,
// which is where a reader is left when the program ends before it has
// handed over what it had. Nothing of the server is left (the harness
// checks), and the model was given no key of what was open.
func TestSessionEndsMidSequence(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for _, st := range openStates {
		t.Run(st.name, func(t *testing.T) {
			hv := newHandover(nil)
			h, tm := startHandover(t, hv, slowLoop(50*time.Microsecond))
			// A key before the prefix, in one write with it: when the
			// model has the key, the library has read the prefix too,
			// and holds it, unfinished, as the session is ended.
			tm.press("z" + st.prefix)
			hv.wait(t, "the model has the key before the prefix", func() bool { return len(hv.keys) > 0 })
			hv.mu.Lock()
			before := strings.Join(hv.keys, "")
			hv.mu.Unlock()
			setKeys(t, h.list)
			if status := tm.exit(); status != 1 {
				t.Errorf("exit status %d, want 1", status)
			}
			if out := tm.stdout.String(); !strings.HasSuffix(out, MsgKeyRemoved+"\r\n") {
				t.Errorf("the session did not end with %q", MsgKeyRemoved)
			}
			// The mark's bytes and the closer's are never the model's.
			hv.mu.Lock()
			after := strings.Join(hv.keys, "")
			hv.mu.Unlock()
			if after != before {
				t.Errorf("the model was given keys after the session was ended: %q then %q", before, after)
			}
			eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
			// The harness's cleanup checks no goroutine is left.
		})
	}
}

// TestFloodEndsLeaveNoReaderWithASlowLoop sends each flood and its mixes
// with a slow event loop and one CPU: the session ends, or is ended
// while the flood that it takes comes, and no reader is left in the
// library, whatever the speed of the loop. A reader left is what the
// tests met on Linux under the race detector.
func TestFloodEndsLeaveNoReaderWithASlowLoop(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	for _, f := range floods {
		t.Run(f.name, func(t *testing.T) {
			hv := newHandover(nil)
			// The end's timeout is not what is tried here: a slow machine
			// is not to reach it with a read's worth of keys in hand.
			h, tm := startHandover(t, hv, slowLoop(50*time.Microsecond), withServer(func(s *Server) { s.endTimeout = longWait }))
			sendFlood(tm, f.start, f.more, 8<<20)
			if f.cut {
				// The session takes this one, and the library makes a key
				// of each byte that the cut lets through, a quarter of a
				// million of them: the session is ended while they come.
				hv.wait(t, "the model has a key of the flood", func() bool { return len(hv.keys) > 0 })
				setKeys(t, h.list)
			}
			if status := tm.exit(); status != 1 {
				t.Errorf("exit status %d, want 1", status)
			}
			eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
			h.noReaderGivenUp(t)
			// The harness's cleanup checks no goroutine is left.
		})
	}
}

// noReaderGivenUp fails the test when the log says that a session's end
// gave its program's reader up.
func (h *harness) noReaderGivenUp(t *testing.T) {
	t.Helper()
	for _, r := range h.log.records() {
		if strings.HasPrefix(r.Msg, "the program's reader did not end in time") {
			t.Errorf("an end gave its reader up: %v", r)
		}
	}
}

// TestSessionEndsWithKeysInHand ends a session while the library's
// reader has thousands of keys to hand over, all of one read, of which
// the program has taken the first: with a slow event loop and one CPU.
// The keys are complete, so nothing says that the library has anything
// gathered, and the end has to wait for the reader all the same: the
// reader is left behind a program that ends before it has taken them.
// Nothing of the server is left (the harness checks), and no end gave
// the reader up.
func TestSessionEndsWithKeysInHand(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	// Fewer than one read of the library's (4096 bytes), and a fifth
	// of a second of events at the pace of the loop.
	keys := strings.Repeat("x", 1000)
	// The model has the first of them: the library has read them all,
	// and has the rest to hand over.
	first := func(t *testing.T, hv *handover) {
		hv.wait(t, "the model has the first key", func() bool { return len(hv.keys) > 0 })
	}
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, h *harness, hv *handover, tm *term)
	}{
		{"the model quits", func(t *testing.T, _ *harness, _ *handover, tm *term) {
			tm.press("q" + keys)
			if status := tm.exit(); status != 0 {
				t.Errorf("exit status %d, want 0", status)
			}
		}},
		{"the key is removed", func(t *testing.T, h *harness, hv *handover, tm *term) {
			tm.press(keys)
			first(t, hv)
			setKeys(t, h.list)
			if status := tm.exit(); status != 1 {
				t.Errorf("exit status %d, want 1", status)
			}
		}},
		{"the client closes the channel", func(t *testing.T, _ *harness, hv *handover, tm *term) {
			tm.press(keys)
			first(t, hv)
			_ = tm.sess.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hv := newHandover(nil)
			// The end's timeout is not what is tried here: a slow machine
			// is not to reach it.
			h, tm := startHandover(t, hv, slowLoop(200*time.Microsecond), withServer(func(s *Server) { s.endTimeout = longWait }))
			tc.end(t, h, hv, tm)
			eventually(t, "the session is gone", func() bool { return h.srv.Sessions() == 0 })
			hv.mu.Lock()
			given := len(hv.keys)
			hv.mu.Unlock()
			if given >= len(keys) {
				t.Fatalf("the model was given %d keys: the session did not end with keys in hand", given)
			}
			h.noReaderGivenUp(t)
			// The harness's cleanup checks no goroutine is left.
		})
	}
}

// TestEndsAreStillFast times an ordinary quit and an end at the table,
// so a change that made winding wait long is seen: an end waits
// windQuiet for its reader, and no more when the reader had nothing in
// hand. One harness, a session opened and ended at a time, so the
// harness's goroutine check does not see many servers at once.
func TestEndsAreStillFast(t *testing.T) {
	const n = 10
	hv := newHandover(nil)
	h := handoverHarness(t, hv)
	var quitTotal, endTotal time.Duration
	for i := range n {
		cl := h.dial(h.key(admin(fmt.Sprintf("q%d", i))), "u")
		tm := openTerm(t, cl)
		tm.waitFor("LASTLINE")
		began := time.Now()
		tm.press("q")
		if tm.exit() != 0 {
			t.Fatal("the model did not quit")
		}
		quitTotal += time.Since(began)
		_ = cl
	}
	// An end at the table: a key of its own, removed, over a list that
	// keeps the quit keys so their connections are not disturbed.
	lines := append([]string(nil), h.lines...)
	for i := range n {
		e := newSigner(t)
		setKeys(t, h.list, append(lines, keyLine(e, admin(fmt.Sprintf("e%d", i))))...)
		tm := openTerm(t, h.dial(e, "u"))
		tm.waitFor("LASTLINE")
		began := time.Now()
		setKeys(t, h.list, lines...)
		if tm.exit() != 1 {
			t.Fatal("the session did not end")
		}
		endTotal += time.Since(began)
	}
	quit, end := quitTotal/n, endTotal/n
	t.Logf("an ordinary quit took %v on average, an end at the table %v, over %d each", quit.Round(time.Microsecond), end.Round(time.Microsecond), n)
	if quit > 500*time.Millisecond || end > 500*time.Millisecond {
		t.Errorf("an end is slow: quit %v, end %v", quit, end)
	}
}
