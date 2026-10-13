package sshd

import (
	"context"
	"errors"
	"io"
	"sync"

	tea "charm.land/bubbletea/v2"
)

// execNotBuilt is Session.Exec until the hand-over is built: done is told
// that it is not.
func execNotBuilt(run func(context.Context, Terminal) error, done func(error) tea.Msg) tea.Cmd {
	return func() tea.Msg { return done(errors.New("not implemented")) }
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

// close ends the input: every reader is told io.EOF once buf is empty,
// and the pump returns when its Read does, which is when the channel is
// closed at the latest.
func (in *input) close() {
	in.mu.Lock()
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
