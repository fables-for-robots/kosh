package sshd

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// The hand-over of a session's terminal to a command, and back.
//
// Bubble Tea's own hand-over, tea.Exec, does the terminal's screens and
// modes, and is used for them: it leaves the alternate screen and the
// program's modes, runs the command in the program's event loop, and
// afterwards sets the modes again and draws the whole frame. What it
// cannot do over an SSH channel is hand over the input (see input), and
// its output is the session's, which translates line feeds. So the
// command is given neither of the two that Bubble Tea offers it: it
// reads a reader of the session's input that is its own for its turn,
// and writes to the channel itself.

// exec is Session.Exec.
func (sess *session) exec(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd {
	// tell tells done what became of the command; a nil done is told
	// nothing, and the program is given no message.
	tell := func(err error) tea.Msg {
		if done == nil {
			return nil
		}
		return done(err)
	}
	return func() tea.Msg {
		if sess.over() {
			return tell(ErrSessionEnded)
		}
		// From here on what the client sends is the command's, and the
		// program's reader is given the mark and its end.
		in, drained, told := sess.in.give()
		if in == nil {
			return tell(ErrTerminalBusy)
		}
		// The mark arrives when the model has every key that was read
		// before the hand-over. It does not arrive when the reader was
		// in the middle of a paste or of an escape sequence: the mark is
		// then a part of that.
		wait := time.NewTimer(drainWait)
		defer wait.Stop()
		select {
		case <-drained:
		case <-wait.C:
		case <-sess.ctx.Done():
		}
		// The reader that was told its end reads no more. One that has
		// not been told yet, because the program was slow to take what
		// it had read, would read next to the reader Bubble Tea starts
		// after the command: two readers of one terminal.
		select {
		case <-told:
		case <-sess.ctx.Done():
		}
		if sess.over() {
			// The program takes no command now.
			sess.in.takeBack(in)
			return tell(ErrSessionEnded)
		}
		return tea.Exec(&turn{sess: sess, run: run, in: in}, func(err error) tea.Msg {
			// Run has taken the terminal back. When Bubble Tea could not
			// release the terminal it has not called Run, and tells
			// this: the terminal is the program's again all the same.
			sess.in.takeBack(in)
			return tell(err)
		})()
	}
}

// minTurn is how long a turn lasts at least: Run does not return sooner,
// however soon the command does.
//
// Bubble Tea v2.0.10 has one ticker for its renderer. When it releases
// the terminal it tells the renderer's goroutine to go, and that
// goroutine stops the ticker on its way out; when it takes the terminal
// back it resets the ticker and starts a new goroutine. Nothing makes
// the first wait for the second: after a command that returns before
// the old goroutine has run, the old goroutine stops the ticker the new
// one draws by, and the program draws nothing from then on, though it
// still takes its keys. The old goroutine has only to be scheduled once,
// which waiting here lets happen: this is time for it, not a guarantee,
// and it costs a command that lasts longer than this nothing. What is
// left to the scheduler is what watchScreen is for.
const minTurn = 10 * time.Millisecond

// turn is the time one command has the terminal, as a tea.ExecCommand:
// Bubble Tea calls Run in the program's event loop when it has released
// the terminal, and takes the terminal back when Run has returned.
type turn struct {
	sess *session
	run  func(context.Context, Terminal) error
	in   *commandInput
}

// SetStdin, SetStdout and SetStderr are given the program's input and
// output, and the standard error of kosh. The command gets none of
// them: see Terminal.
func (t *turn) SetStdin(io.Reader) {}

// SetStdout is SetStdin for the output.
func (t *turn) SetStdout(io.Writer) {}

// SetStderr is SetStdin for the error stream.
func (t *turn) SetStderr(io.Writer) {}

// Run calls the command with the terminal, and returns what it
// returned. When Run returns the turn is over: the command's reader is
// told io.EOF, its writer is closed, and the input is the program's, for
// the reader Bubble Tea starts next.
//
// The command's context is done when the session ends, by the client or
// by kosh, and from that moment the command reads io.EOF and its writes
// fail, though the turn lasts until the command has returned. A session
// that ended before Run is called gives ErrSessionEnded, and the command
// is not called.
func (t *turn) Run() error {
	sess := t.sess
	began := time.Now()
	out := &gate{w: sess.raw}
	ctx, cancel := context.WithCancel(sess.ctx)
	// In a goroutine of its own: closing the gate waits for a write of
	// the command to a client that does not read.
	stop := context.AfterFunc(ctx, func() {
		sess.in.cut(t.in)
		out.close()
	})
	defer func() {
		stop()
		cancel()
		// A write of the command's that is under way is waited for: the
		// program's output must not run into it. One that the client
		// does not take ends with the channel or the connection.
		sess.commandWaits(waitsForWrite)
		out.close()
		sess.commandWaits("")
		sess.in.takeBack(t.in)
		// See minTurn. What the command left unread waits for the
		// program.
		if rest := sess.srv.minTurn - time.Since(began); rest > 0 {
			time.Sleep(rest)
		}
		sess.watchScreen()
	}()
	if !sess.beginCommand(cancel) {
		return ErrSessionEnded
	}
	size, resized, unwatch := sess.win.watch()
	defer unwatch()
	return t.run(ctx, Terminal{In: t.in, Out: out, Size: size, Resized: resized})
}

// gate is the writer of one command's turn: it passes what it is given
// on to w, and after close it passes nothing.
type gate struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

// Write writes p to w, or fails with io.ErrClosedPipe after close.
func (g *gate) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return 0, io.ErrClosedPipe
	}
	return g.w.Write(p)
}

// close closes the gate. It waits for a Write that is under way: when
// it returns nothing more of the command's reaches the terminal.
func (g *gate) close() {
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
}

// filter is the program's filter. It does what the filter of wish did,
// which it replaces: a suspension becomes a resumption, since there is
// no process to suspend. And it takes drainMark out, whoever sent it:
// the model is never given it.
func (sess *session) filter(_ tea.Model, msg tea.Msg) tea.Msg {
	switch msg := msg.(type) {
	case tea.SuspendMsg:
		return tea.ResumeMsg{}
	case tea.ModeReportMsg:
		if msg.Mode != nil && msg.Mode.Mode() == drainMode {
			sess.in.markSeen()
			return nil
		}
	}
	return msg
}

// The session channel itself.

// channelKey is the key under which a session channel's context has the
// channel's accepted.
type channelKey struct{}

// accepted holds the channel that filteredChannel accepted.
type accepted struct {
	mu sync.Mutex
	ch gossh.Channel
}

func (a *accepted) set(ch gossh.Channel) {
	a.mu.Lock()
	a.ch = ch
	a.mu.Unlock()
}

// channelContext is a connection's context for one of its session
// channels. The library keeps the channel it accepts to itself, and
// gives the session the context it was given: through this one the
// session's handler finds the channel. Everything else, the values that
// are set too, is the connection's context.
type channelContext struct {
	ssh.Context
	a *accepted
}

// Value returns the channel's accepted for channelKey, and the
// connection's value for any other key.
func (c channelContext) Value(key any) any {
	if _, ok := key.(channelKey); ok {
		return c.a
	}
	return c.Context.Value(key)
}

// rawWriter returns the session channel of ss, which writes what it is
// given. ss writes through the library's emulated terminal, which makes
// every line feed a carriage return and a line feed: right for the
// program, whose renderer counts on it, and wrong for anything that
// draws by itself. A session whose channel is not known, which there is
// none of since every session channel comes through sessionChannel, gets
// ss.
func rawWriter(ss ssh.Session) io.Writer {
	if a, ok := ss.Context().Value(channelKey{}).(*accepted); ok {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.ch != nil {
			return a.ch
		}
	}
	return ss
}

// The input of a session.

const (
	// inputBuffer is how many bytes of a session's input wait to be read,
	// at most. When that many wait the channel is not read, and SSH's
	// flow control holds the client up as it did when the program read
	// the channel itself.
	inputBuffer = 32 << 10
	// pumpChunk is how many bytes the pump asks the channel for at once.
	pumpChunk = 4 << 10
)

// drainMark is the last thing the program's reader is given before a
// command has the terminal: the report of a terminal mode that no
// terminal has. Bubble Tea's read loop makes messages of what it has
// read and gives them to the program one by one, in order, so when this
// report arrives at the program, which is in the session's filter, every
// key the reader had read before the hand-over has been given to the
// model.
//
// Without it a key still on its way holds up the hand-over: the read
// loop waits for the program to take the key, the program, which is
// releasing the terminal, waits half a second for the read loop to end,
// and the key reaches the model after the command, out of its order.
const (
	drainMode = 28657
	drainMark = "\x1b[?28657;0$y"
	// drainWait is how long a hand-over waits for the mark to arrive.
	drainWait = 250 * time.Millisecond
)

// input is what the client sends on the session channel. One goroutine,
// pump, reads the channel; the bytes wait in buf, inputBuffer of them at
// most, for the one who has the terminal: the program, or the command
// the program gave the terminal to. turn says which, and nobody else is
// given a byte, so no byte is ever in the hands of a reader that may not
// keep it.
//
// The program is not given the channel to read, because Bubble Tea
// cannot take a reader that is not a file out of a Read: when it
// releases the terminal its reader stays in Read, next to the command's,
// and throws away the next thing it is given. The program reads a
// programInput instead, whose Read ends, with io.EOF, when a command has
// the terminal: Bubble Tea's read loop is over before the command runs,
// and Bubble Tea starts a new one when the command has returned.
type input struct {
	mu sync.Mutex
	// cond is told of every change of what is below.
	cond *sync.Cond
	buf  []byte
	// err is why no more will come once buf is empty: the channel's
	// error, or io.EOF when the input was closed.
	err error
	// gone is set when the program's reader was told err: the program
	// reads no more.
	gone bool

	// turn is the reader of the command that has the terminal, or nil
	// when the program has it.
	turn *commandInput
	// mark is set while the program's reader is owed drainMark, before
	// its io.EOF.
	mark bool
	// drained is closed when drainMark has arrived at the program, and
	// told when the program's reader has been told io.EOF. They are nil
	// when the program has the terminal, and once closed.
	drained, told chan struct{}
}

func newInput() *input {
	in := &input{}
	in.cond = sync.NewCond(&in.mu)
	return in
}

// pump reads r into buf until r fails or the input is closed. It does
// not read while buf is full.
func (in *input) pump(r io.Reader) {
	chunk := make([]byte, pumpChunk)
	for {
		in.mu.Lock()
		for len(in.buf) >= inputBuffer && in.err == nil {
			in.cond.Wait()
		}
		// Only this goroutine adds to buf: there is room for this much
		// when Read returns, too.
		room := inputBuffer - len(in.buf)
		closed := in.err != nil
		in.mu.Unlock()
		if closed {
			return
		}
		n, err := r.Read(chunk[:min(len(chunk), room)])
		in.mu.Lock()
		if in.err != nil {
			// Closed meanwhile: the session has ended.
			in.mu.Unlock()
			return
		}
		in.buf = append(in.buf, chunk[:n]...)
		in.err = err
		in.cond.Broadcast()
		in.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// close ends the input: what waits in buf is dropped, every reader is
// told the end from now on (io.EOF, or the channel's error when it had
// one), and the pump returns when its Read does, which is when the
// channel is closed at the latest.
func (in *input) close() {
	in.mu.Lock()
	in.buf = nil
	if in.err == nil {
		in.err = io.EOF
	}
	in.cond.Broadcast()
	in.mu.Unlock()
}

// read is Read for the program, when c is nil, or for the command c.
func (in *input) read(p []byte, c *commandInput) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	in.mu.Lock()
	defer in.mu.Unlock()
	for {
		switch {
		case c == nil && in.turn != nil:
			// The program's reader while a command has the terminal:
			// the mark once, then the end, and no byte of the client's.
			if in.mark && len(p) >= len(drainMark) {
				in.mark = false
				return copy(p, drainMark), nil
			}
			in.mark = false
			if in.told != nil {
				close(in.told)
				in.told = nil
			}
			return 0, io.EOF
		case c != nil && (in.turn != c || c.over):
			// A command's reader after its turn.
			return 0, io.EOF
		case len(in.buf) > 0:
			n := copy(p, in.buf)
			in.buf = in.buf[n:]
			if len(in.buf) == 0 {
				// Nothing waits: the memory is given back.
				in.buf = nil
			}
			in.cond.Broadcast()
			return n, nil
		case in.err != nil:
			if c != nil {
				return 0, io.EOF
			}
			in.gone = true
			return 0, in.err
		}
		in.cond.Wait()
	}
}

// give gives the terminal's input to a command, whose reader it
// returns: from now on the program's reader is given drainMark and then
// io.EOF, and what the client sends is the command's. drained is closed
// when the mark has arrived at the program (see markSeen), told when the
// program's reader has been told io.EOF; both are closed already when
// the program reads no more. give returns nil when a command has the
// terminal.
func (in *input) give() (c *commandInput, drained, told <-chan struct{}) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.turn != nil {
		return nil, nil, nil
	}
	c = &commandInput{in: in}
	in.turn = c
	d, t := make(chan struct{}), make(chan struct{})
	if in.gone {
		close(d)
		close(t)
	} else {
		in.mark, in.drained, in.told = true, d, t
	}
	in.cond.Broadcast()
	return c, d, t
}

// markSeen is called when drainMark has arrived at the program.
func (in *input) markSeen() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.drained != nil {
		close(in.drained)
		in.drained = nil
	}
}

// cut ends c's reading before its turn is over: c is told io.EOF from
// now on, also in a Read that is waiting. The terminal stays c's until
// takeBack: what c has not read waits for the program.
func (in *input) cut(c *commandInput) {
	in.mu.Lock()
	c.over = true
	in.cond.Broadcast()
	in.mu.Unlock()
}

// takeBack ends c's turn: c is told io.EOF from now on, also in a Read
// that is waiting, and the program has the terminal again, with what c
// has not read.
func (in *input) takeBack(c *commandInput) {
	in.mu.Lock()
	defer in.mu.Unlock()
	c.over = true
	if in.turn == c {
		in.turn, in.mark, in.drained, in.told = nil, false, nil, nil
	}
	in.cond.Broadcast()
}

// programInput is the program's reader of an input.
type programInput struct{ in *input }

// Read gives the program what the client sent while the program has the
// terminal. When a command has it, Read returns drainMark once and then
// io.EOF, at once.
func (p programInput) Read(b []byte) (int, error) { return p.in.read(b, nil) }

// commandInput is the reader of an input for one command's turn.
type commandInput struct {
	in *input
	// over is set, under in.mu, when the command reads no more: its turn
	// has ended, or was cut.
	over bool
}

// Read gives the command what the client sent in its turn, and io.EOF
// from the end of the turn on.
func (c *commandInput) Read(b []byte) (int, error) { return c.in.read(b, c) }

// The program's screen after a turn.

// screenCheck is how long after a turn the session looks whether its
// program has written.
const screenCheck = 150 * time.Millisecond

// countingWriter is a writer whose calls of Write are counted.
type countingWriter struct {
	w io.Writer
	// n is how many times Write was called, also by a call that has not
	// returned.
	n atomic.Int64
}

// Write writes p to w.
func (c *countingWriter) Write(p []byte) (int, error) {
	c.n.Add(1)
	return c.w.Write(p)
}

// watchScreen is called at the end of a turn, before Bubble Tea takes
// the terminal back: in screenCheck from now the session looks whether
// its program has written since.
//
// A program that has the terminal back writes: the alternate screen's
// sequence and its frame, with the renderer's first tick. One that has
// not written by then has the dead renderer that minTurn tells of, and
// stays so: a key does not wake it, a new size does not. Another
// hand-over does, since it starts the renderer anew.
func (sess *session) watchScreen() {
	n := sess.out.n.Load()
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.overLocked() {
		return
	}
	if sess.screenTimer != nil {
		sess.screenTimer.Stop()
	}
	sess.screenTimer = time.AfterFunc(screenCheck, func() { sess.checkScreen(n) })
}

// checkScreen repairs the program's screen when the program has written
// nothing since its output had n writes: it hands the terminal over once
// more, to a command that only waits, and tells nobody but the log. The
// end of that turn has the screen looked at again. Nothing is done when
// the program has written, when the session is over, or when a command
// has the terminal: that command's turn ends with a look of its own.
func (sess *session) checkScreen(n int64) {
	if sess.out.n.Load() != n || sess.over() {
		return
	}
	// As the program would run the command of a hand-over: it returns
	// nil when the terminal is busy or the session over.
	msg := sess.exec(func(ctx context.Context, _ Terminal) error {
		// Not the server's minTurn, which a test may have made nothing.
		wait := time.NewTimer(minTurn)
		defer wait.Stop()
		select {
		case <-wait.C:
		case <-ctx.Done():
		}
		return nil
	}, nil)()
	if msg == nil {
		return
	}
	sess.mu.Lock()
	p := sess.program
	sess.mu.Unlock()
	sess.srv.log.Warn("the program's screen was repaired after a command",
		"transport", sess.conn.transport, "remote", sess.conn.remote,
		"key", sess.grant.Name, "fingerprint", fingerprint(sess.key))
	// Send returns when the program takes it, or has ended.
	p.Send(msg)
}
