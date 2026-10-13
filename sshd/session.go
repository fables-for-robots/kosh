package sshd

import (
	"context"
	"io"
	"log/slog"
	"sync"
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
	s.log.Info("session refused", "transport", c.transport, "remote", c.remote, "reason", msg)
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

	mu sync.Mutex
	// reason is why kosh ends the session; "" while it does not.
	reason string
}

// run gives the session its program, and ends it when the program ends.
func (s *Server) run(c *conn, ss ssh.Session) {
	ctx, cancel := context.WithCancel(ss.Context())
	defer cancel()
	sess := &session{srv: s, conn: c, ss: ss, key: ss.PublicKey(), ctx: ctx}
	s.register(sess)
	defer s.unregister(sess)
	log := s.log.With("transport", c.transport, "remote", c.remote,
		"key", sess.grant.Name, "fingerprint", fingerprint(sess.key), "read_only", sess.grant.ReadOnly)

	if sess.ending() == "" {
		m := s.cfg.Model(Session{Context: ctx, Grant: sess.grant, Transport: c.transport, Remote: c.remote})
		opts := append(bm.MakeOptions(ss),
			// The program ends with the session, however it ends.
			tea.WithContext(ctx),
			// SIGINT and SIGTERM are kosh's, not a session's.
			tea.WithoutSignalHandler(),
		)
		p := tea.NewProgram(m, opts...)

		s.mu.Lock()
		s.running++
		s.mu.Unlock()
		log.Info("session start")
		began := time.Now()

		go forwardWindow(ctx, ss, p)
		_, _ = p.Run()

		s.mu.Lock()
		s.running--
		s.mu.Unlock()
		defer func() { logEnd(log, began, sess.ending()) }()
	}

	if reason := sess.ending(); reason != "" {
		_, _ = io.WriteString(ss, reason+"\n")
		_ = ss.Exit(1)
		return
	}
	_ = ss.Exit(0)
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

// forwardWindow tells p each new size of the session's window, until ctx
// is done.
func forwardWindow(ctx context.Context, ss ssh.Session, p *tea.Program) {
	_, windows, _ := ss.Pty()
	for {
		select {
		case <-ctx.Done():
			return
		case w, ok := <-windows:
			if !ok {
				return
			}
			p.Send(tea.WindowSizeMsg{Width: w.Width, Height: w.Height})
		}
	}
}
