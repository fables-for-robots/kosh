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
}

// run gives the session its program, and ends it when the program ends.
func (s *Server) run(c *conn, ss ssh.Session) {
	ctx, cancel := context.WithCancel(ss.Context())
	defer cancel()
	// The session ends when the client closes its channel, even if the
	// connection stays: nothing would end the program then.
	win := s.windowOf(ss)
	go func() {
		select {
		case <-win.closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	sess := &session{srv: s, conn: c, ss: ss, key: ss.PublicKey(), ctx: ctx}
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
			return s.cfg.Model(Session{Context: ctx, Grant: sess.grant, Transport: c.transport, Remote: c.remote})
		}, &sess.stopped, panicked)
	}
	if m != nil {
		opts := append(bm.MakeOptions(ss),
			// The program ends with the session, however it ends.
			tea.WithContext(ctx),
			// SIGINT and SIGTERM are kosh's, not a session's.
			tea.WithoutSignalHandler(),
		)
		p := tea.NewProgram(m, opts...)
		if started = sess.start(p); started {
			s.mu.Lock()
			s.running++
			s.mu.Unlock()
			log.Info("session start")
			began := time.Now()

			go win.forward(ctx, p)
			_, err := p.Run()
			if errors.Is(err, tea.ErrProgramPanic) {
				// A command's goroutine panicked, and Bubble Tea caught
				// it and printed the stack to the standard error.
				log.Error("session's program panicked in a command", "err", err)
				sess.end(MsgCrashed)
			}

			s.mu.Lock()
			s.running--
			s.mu.Unlock()
			defer func() { logEnd(log, began, sess.ending()) }()
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
// status 1. end does not wait for any of that. A session that has not
// ended within srv.endTimeout has its connection closed. end reports
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
	sess.timer = time.AfterFunc(sess.srv.endTimeout, func() {
		sess.srv.log.Warn("session did not end in time: its connection is closed",
			"transport", sess.conn.transport, "remote", sess.conn.remote, "key", sess.grant.Name, "reason", msg)
		_ = sess.conn.Close()
	})
	if p := sess.program; p != nil {
		// Quit waits for the program to take it, and the program may be
		// waiting for a client that does not read.
		go p.Quit()
	}
	return true
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
}

// fingerprint is key's SHA256 fingerprint, or "" for no key.
func fingerprint(key ssh.PublicKey) string {
	if key == nil {
		return ""
	}
	return gossh.FingerprintSHA256(key)
}

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
// full to read-only or back with MsgGrantChanged. It does not wait for
// any session: it is called in the goroutine of the list's Replace.
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
