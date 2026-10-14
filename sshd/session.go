package sshd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/ssh"
	bm "charm.land/wish/v2/bubbletea"
	gossh "golang.org/x/crypto/ssh"

	"github.com/fables-for-robots/kosh/keys"
)

// handle is the library's handler: it is called for a session that asks
// for a shell or a command, and the session ends when it returns.
func (s *Server) handle(ss ssh.Session) {
	c := connOf(ss.RemoteAddr())
	if c == nil {
		// Every connection comes through Serve, which makes it a conn.
		_ = ss.Exit(1)
		return
	}
	c.sessionStarted()
	defer c.sessionEnded()
	if sc, ok := ss.Context().Value(ssh.ContextKeyConn).(gossh.Conn); ok {
		c.startKeepalive(ss.Context(), sc)
	}

	if ss.RawCommand() != "" {
		s.refuse(c, ss, MsgNoCommands)
		return
	}
	if _, _, ok := ss.Pty(); !ok {
		s.refuse(c, ss, MsgNeedsTerminal)
		return
	}
	s.run(c, ss)
}

// refuse ends a session before it has a program: msg and a line feed on
// its error stream, and exit status 1.
func (s *Server) refuse(c *conn, ss ssh.Session, msg string) {
	key := ss.PublicKey()
	g, _ := s.cfg.Keys.Lookup(key)
	s.log.Info("session refused", "transport", c.transport, "remote", c.remote,
		"key", g.Name, "fingerprint", fingerprint(key), "reason", msg)
	_, _ = io.WriteString(ss.Stderr(), msg+"\n")
	_ = ss.Exit(1)
}

// session is a session with a terminal, from the moment its key is
// looked up again to its end.
type session struct {
	srv  *Server
	conn *conn
	ss   ssh.Session
	key  ssh.PublicKey
	ctx  context.Context

	// grant is the key's grant when the session began. It is set under
	// srv.mu, before the session is in srv.sessions, and not changed.
	grant keys.Grant

	// stopped is set when kosh ends the session; the model is given no
	// message after that.
	stopped atomic.Bool
	// channelClosed is set when the client closes the session channel.
	channelClosed atomic.Bool

	mu sync.Mutex
	// reason is why kosh ends the session; "" while it does not.
	reason string
	// program is the session's program, once it is made.
	program *tea.Program
	// timer closes the connection when the session takes longer than
	// srv.endTimeout to end after kosh ended it.
	timer *time.Timer
	// finished is set when the session has exited: end does nothing
	// after that.
	finished bool
	// cancelCommand ends the context of the command that has the
	// terminal; it is nil when none has.
	cancelCommand context.CancelFunc
	// waits is what a turn that is not over waits for: waitsForCommand,
	// waitsForWrite, or "" when no turn is under way or nothing is
	// waited for.
	waits string
	// turnOut is the output of the command that has the terminal, or
	// nil.
	turnOut *gate

	// in is what the client sends, for whoever has the terminal. It is
	// read from the channel from the moment the program starts.
	in *input
	// win is the session's window.
	win *window
	// raw writes to the session channel what it is given: ss does not.
	raw io.Writer
	// out is the program's output, which is ss, with its writes counted.
	out *countingWriter
	// windOnce, winding and wound are the end of the program's reading,
	// which comes before the program's end: see wind.
	windOnce sync.Once
	winding  atomic.Bool
	wound    atomic.Bool
	// ran is closed when the program has returned: nothing takes an
	// event of its reader's from then on.
	ran chan struct{}
	// lastEvent is when the filter last saw an event of the library's
	// reader, or when that reader was told its end if that was later, in
	// Unix nanoseconds: see wind.
	lastEvent atomic.Int64

	// screenTimer looks, after a command's turn, whether the program
	// writes again: see watchScreen. It is nil before the first turn.
	screenTimer *time.Timer
}

// run gives the session its program, and ends it when the program ends.
func (s *Server) run(c *conn, ss ssh.Session) {
	ctx, cancel := context.WithCancel(ss.Context())
	defer cancel()
	// The session ends when the client closes its channel, even if the
	// connection stays: nothing would end the program then.
	sess := &session{srv: s, conn: c, ss: ss, key: ss.PublicKey(), ctx: ctx, in: newInput(), out: &countingWriter{w: ss}, ran: make(chan struct{})}
	win := s.windowOf(ss)
	sess.win, sess.raw = win, rawWriter(ss)
	go func() {
		select {
		case <-win.closed:
			sess.channelClosed.Store(true)
			cancel()
			sess.channelGone()
		case <-ctx.Done():
		}
	}()
	s.register(sess)
	defer s.unregister(sess)
	log := s.log.With("transport", c.transport, "remote", c.remote,
		"key", sess.grant.Name, "fingerprint", fingerprint(sess.key), "read_only", sess.grant.ReadOnly)

	panicked := func(in string, value any, stack []byte) {
		log.Error("session's program panicked", "in", in, "panic", fmt.Sprint(value), "stack", string(stack))
		sess.end(MsgCrashed)
	}
	var m tea.Model
	started := false
	if sess.ending() == "" {
		m = makeModel(func() tea.Model {
			return s.cfg.Model(Session{Context: ctx, Grant: sess.grant, Transport: c.transport, Remote: c.remote, Exec: sess.exec})
		}, &sess.stopped, panicked)
	}
	if m != nil {
		// The program's own context, which is done after the session's.
		pctx, endProgram := context.WithCancel(context.Background())
		defer endProgram()
		opts := append(bm.MakeOptions(ss),
			// The program ends with the session, however it ends: when
			// the session's context is done, after its reader (below).
			tea.WithContext(pctx),
			// SIGINT and SIGTERM are kosh's, not a session's.
			tea.WithoutSignalHandler(),
			// The channel is read by the session, and the program is
			// given what is the program's: see input.
			tea.WithInput(programInput{sess.in}),
			// In place of the filter among MakeOptions.
			tea.WithFilter(sess.filter),
			// In place of the output among them, which is ss.
			tea.WithOutput(sess.out),
		)
		p := tea.NewProgram(m, opts...)
		if started = sess.start(p); started {
			sess.in.flood = sess.flooded
			go sess.in.pump(ss)
			go func() {
				select {
				case <-ctx.Done():
					sess.wind()
					endProgram()
				case <-pctx.Done():
				}
			}()
			s.mu.Lock()
			s.running++
			s.mu.Unlock()
			log.Info("session start")
			began := time.Now()

			go win.forward(ctx, p)
			_, err := p.Run()
			close(sess.ran)
			// A reader that Bubble Tea left in Read is told the end, and
			// gets nothing of what waits; the pump ends with the channel.
			sess.in.close()
			// One goroutine of Bubble Tea's can be left, with what it had
			// to hand to the program, when the program took no message at
			// its end or ended by a panic (see wind): it holds the guard,
			// and not the model with all it has.
			if g, ok := m.(*guard); ok {
				g.release()
			}
			// Taken now: a channel that closes after the program ended
			// by itself is not why it ended.
			byClient := sess.channelClosed.Load()
			if errors.Is(err, tea.ErrProgramPanic) {
				// A command's goroutine panicked, and Bubble Tea caught
				// it and printed the stack to the standard error.
				log.Error("session's program panicked in a command", "err", err)
				sess.end(MsgCrashed)
			}

			s.mu.Lock()
			s.running--
			s.mu.Unlock()
			defer func() {
				reason := sess.ending()
				if reason == "" && byClient {
					reason = reasonChannelClosed
				}
				logEnd(log, began, reason)
			}()
		}
	}

	// The program has stopped and given the terminal back, or there was
	// none.
	defer sess.finish()
	if !started {
		log.Info("session refused", "reason", sess.ending())
	}
	if reason := sess.ending(); reason != "" {
		_, _ = io.WriteString(ss, reason+"\n")
		_ = ss.Exit(1)
		return
	}
	_ = ss.Exit(0)
}

// start makes p the session's program, unless kosh has ended the session
// already.
func (sess *session) start(p *tea.Program) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.reason != "" {
		return false
	}
	sess.program = p
	return true
}

// end ends the session from kosh's side: it stops the program, and when
// the program has stopped and the terminal is back to its normal screen,
// msg and a line feed are written to the session and it exits with
// status 1. A command that has the terminal has its context ended, and
// the program stops when the command has returned. end does not wait
// for any of that. A session that has not ended within srv.endTimeout,
// because its client does not read or its command does not return, has
// its connection closed. end reports
// whether it ended the session; a session that was ended before keeps
// its first reason.
func (sess *session) end(msg string) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.reason != "" || sess.finished {
		return false
	}
	sess.reason = msg
	sess.stopped.Store(true)
	if sess.timer != nil {
		// channelGone's: this one is as good, and finish stops one.
		sess.timer.Stop()
	}
	sess.timer = time.AfterFunc(sess.srv.endTimeout, func() { sess.cutOff(msg) })
	if sess.cancelCommand != nil {
		// The program takes nothing, Quit neither, until the command
		// that has the terminal has returned.
		sess.cancelCommand()
	}
	if sess.program != nil {
		go sess.quit()
	}
	return true
}

// What a turn waits for, in the log.
const (
	waitsForCommand = "the command that has its terminal did not return"
	waitsForWrite   = "the client did not take what its command wrote"
)

// cutOff closes the connection of a session that was to end, for reason,
// and has not: because its client does not read what the program or a
// command writes, or because its command does not return. The log says
// which.
func (sess *session) cutOff(reason string) {
	what := "session did not end in time: its connection is closed"
	sess.mu.Lock()
	waits := sess.waits
	if waits == waitsForCommand && sess.turnOut.writing.Load() > 0 {
		// The command has not returned because it waits for its write,
		// or it will not be long: the write is what holds the session.
		waits = waitsForWrite
	}
	sess.mu.Unlock()
	if waits != "" {
		what = "session did not end in time, " + waits + ": its connection is closed"
	}
	sess.srv.log.Warn(what,
		"transport", sess.conn.transport, "remote", sess.conn.remote,
		"key", sess.grant.Name, "fingerprint", fingerprint(sess.key), "reason", reason)
	_ = sess.conn.Close()
}

// channelGone is called when the client has closed the session channel
// and the session's context is done. A program ends with its context; a
// command that has the terminal is to return on its own, and when it
// does not, nothing else would end the session of a client that keeps
// its connection: the connection is closed after srv.endTimeout, as it
// is when kosh ends a session.
func (sess *session) channelGone() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.cancelCommand == nil || sess.timer != nil || sess.finished {
		return
	}
	sess.timer = time.AfterFunc(sess.srv.endTimeout, func() { sess.cutOff(reasonChannelClosed) })
}

// over reports whether the session has ended or is being ended: by the
// client, by kosh, or because its program has returned.
func (sess *session) over() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.overLocked()
}

// overLocked is over with sess.mu held.
func (sess *session) overLocked() bool {
	return sess.reason != "" || sess.finished || sess.ctx.Err() != nil || sess.winding.Load()
}

// beginCommand makes cancel what ends the context of the command that
// has the terminal, and out that command's output. It reports false, and
// keeps nothing, when the session is over: end, which would call cancel,
// has been and gone.
func (sess *session) beginCommand(cancel context.CancelFunc, out *gate) bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.overLocked() {
		return false
	}
	sess.cancelCommand, sess.waits, sess.turnOut = cancel, waitsForCommand, out
	return true
}

// commandWaits is called when the command that has the terminal has
// returned, with what its turn waits for now: waitsForWrite, and then
// "" when the turn waits for nothing more.
func (sess *session) commandWaits(what string) {
	sess.mu.Lock()
	sess.cancelCommand, sess.waits = nil, what
	if what == "" {
		sess.turnOut = nil
	}
	sess.mu.Unlock()
}

// finish marks the session as exited, and stops the timer of end. An end
// that comes later, from a change of the key list that saw the session
// before it was gone, does nothing: its timer would close a connection
// that may have a new session by then.
func (sess *session) finish() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	sess.finished = true
	if sess.timer != nil {
		sess.timer.Stop()
	}
	if sess.screenTimer != nil {
		sess.screenTimer.Stop()
	}
}

// fingerprint is key's SHA256 fingerprint, or "" for no key.
func fingerprint(key ssh.PublicKey) string {
	if key == nil {
		return ""
	}
	return gossh.FingerprintSHA256(key)
}

// reasonChannelClosed is why a session ended when its client closed the
// channel, in the log.
const reasonChannelClosed = "the client closed the channel"

func logEnd(log *slog.Logger, began time.Time, reason string) {
	args := []any{"duration", time.Since(began).Round(time.Millisecond)}
	if reason != "" {
		args = append(args, "reason", reason)
	}
	log.Info("session end", args...)
}

// register looks the session's key up again and puts the session among
// the open ones. A key that is gone, and a server that is shutting down,
// end the session before it begins.
func (s *Server) register(sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Under s.mu, as keysChanged looks: a change of the list is seen
	// either here or there.
	g, ok := s.cfg.Keys.Lookup(sess.key)
	sess.grant = g
	switch {
	case s.closing:
		sess.reason = MsgShuttingDown
	case !ok:
		sess.reason = MsgKeyRemoved
	}
	s.sessions[sess] = struct{}{}
	s.notifyLocked()
}

// keysChanged is called when the key list has changed. A session whose
// key is gone is ended with MsgKeyRemoved, and one whose key went from
// full to read-only or back with MsgGrantChanged; then a connection whose
// key is gone is closed once it has no session. It does not wait for any
// session: it is called in the goroutine of the list's Replace.
func (s *Server) keysChanged() {
	type ending struct {
		sess *session
		msg  string
	}
	var ends []ending
	s.mu.Lock()
	for sess := range s.sessions {
		g, ok := s.cfg.Keys.Lookup(sess.key)
		switch {
		case !ok:
			ends = append(ends, ending{sess, MsgKeyRemoved})
		case g.ReadOnly != sess.grant.ReadOnly:
			ends = append(ends, ending{sess, MsgGrantChanged})
		}
	}
	s.mu.Unlock()
	removed, changed := 0, 0
	for _, e := range ends {
		if !e.sess.end(e.msg) {
			continue
		}
		if e.msg == MsgKeyRemoved {
			removed++
		} else {
			changed++
		}
	}
	if removed+changed > 0 {
		s.log.Info("key list changed: sessions are closed", "key_removed", removed, "grant_changed", changed)
	}

	// The connections of a key that left the list are closed, each when
	// it has no session; a key whose grant changed keeps its own. c.key
	// is read under s.mu after countKey set keyFingerprint under it.
	var gone []*conn
	s.mu.Lock()
	for c := range s.conns {
		if c.keyFingerprint == "" {
			continue
		}
		if _, ok := s.cfg.Keys.Lookup(c.key); !ok {
			gone = append(gone, c)
		}
	}
	s.mu.Unlock()
	for _, c := range gone {
		c.keyRemoved()
	}
}

func (s *Server) unregister(sess *session) {
	s.mu.Lock()
	delete(s.sessions, sess)
	s.notifyLocked()
	s.mu.Unlock()
}

// ending returns why kosh ends the session, or "".
func (sess *session) ending() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.reason
}

// window is the window of a session channel that asked for a terminal.
//
// The library puts the size of each window-change request on a channel
// with room for one, and blocks its request loop on it when it is full:
// a client could stop the loop, and leave its goroutine behind for good,
// with two window changes nobody takes. So the channel is drained from
// the moment pty-req makes it until the library closes it with the
// session channel; the latest size is kept, for the program when one
// runs.
type window struct {
	// closed is closed when the session channel is.
	closed chan struct{}
	// changed has a value when size has changed since it was taken.
	changed chan struct{}

	mu   sync.Mutex
	size ssh.Window
	// watcher is given a value, when it has none, with each change of
	// size: it is the Resized of the command that has the terminal, or
	// nil.
	watcher chan struct{}
}

// ptyHandler is the library's PtyHandler: the library's emulated
// terminal, whose window is drained at once.
func (s *Server) ptyHandler(emulate ssh.PtyHandler) ssh.PtyHandler {
	return func(ctx ssh.Context, ss ssh.Session, pty ssh.Pty) (func() error, error) {
		closer, err := emulate(ctx, ss, pty)
		if err != nil {
			return nil, err
		}
		_, sizes, ok := ss.Pty()
		if !ok {
			return closer, nil
		}
		w := &window{closed: make(chan struct{}), changed: make(chan struct{}, 1)}
		s.mu.Lock()
		s.windows[ss] = w
		s.mu.Unlock()
		go func() {
			for size := range sizes {
				w.mu.Lock()
				w.size = size
				if w.watcher != nil {
					select {
					case w.watcher <- struct{}{}:
					default:
					}
				}
				w.mu.Unlock()
				select {
				case w.changed <- struct{}{}:
				default:
				}
			}
			s.mu.Lock()
			delete(s.windows, ss)
			s.mu.Unlock()
			close(w.closed)
		}()
		return closer, nil
	}
}

// closedWindow is the window of a session channel that is closed.
var closedWindow = func() *window {
	w := &window{closed: make(chan struct{}), changed: make(chan struct{})}
	close(w.closed)
	return w
}()

// windowOf returns the window of ss. pty-req makes it before the shell
// or exec request that starts the handler, in the same request loop, and
// it is forgotten only when the channel is closed: a session without one
// has a closed channel.
func (s *Server) windowOf(ss ssh.Session) *window {
	s.mu.Lock()
	defer s.mu.Unlock()
	if w, ok := s.windows[ss]; ok {
		return w
	}
	return closedWindow
}

// forward tells p each new size of the window, until ctx is done.
func (w *window) forward(ctx context.Context, p *tea.Program) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.closed:
			return
		case <-w.changed:
			w.mu.Lock()
			size := w.size
			w.mu.Unlock()
			// Send returns when the program takes it, or has ended.
			p.Send(tea.WindowSizeMsg{Width: size.Width, Height: size.Height})
		}
	}
}

// watch returns a Terminal's Size and Resized for the turn of a command,
// and what ends them. The program cannot tell the command of a size: it
// takes no message while a command runs, and forward waits in Send with
// the first change. So the command asks the window: size returns the
// size now, and resized has a value when the size has changed since size
// was last called, one however many changes there were. After stop
// resized gets no more.
func (w *window) watch() (size func() (width, height int), resized <-chan struct{}, stop func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	w.watcher = ch
	w.mu.Unlock()
	size = func() (int, int) {
		w.mu.Lock()
		defer w.mu.Unlock()
		// Under the lock a change is made under: the size returned is
		// the one this value was for, or a later one.
		select {
		case <-ch:
		default:
		}
		return w.size.Width, w.size.Height
	}
	stop = func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.watcher == ch {
			w.watcher = nil
		}
	}
	return size, ch, stop
}
