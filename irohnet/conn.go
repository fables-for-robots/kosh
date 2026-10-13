package irohnet

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tmc/go-iroh/iroh"
)

// conn is the one stream of one iroh connection as a net.Conn. Reads,
// writes and deadlines are the stream's; closing it closes the stream and
// then the iroh connection.
type conn struct {
	stream net.Conn
	qc     *iroh.Conn
	// ep is the endpoint DialLocal bound for the connection, shut down
	// with it; nil for a connection of a Listener.
	ep *iroh.Endpoint

	local, remote Addr
	// linger is how long, after the stream is closed, the peer is given
	// to close the connection before it is closed from this side:
	// closing it at once would drop what was written last and is not
	// yet acknowledged. spawn runs the wait in a goroutine its owner
	// counts. stop is done once the listener is closing: a Close after
	// that, and one without spawn, closes the connection at once. A wait
	// that has begun goes on when the listener closes, which waits for
	// it; only closeNow ends it early.
	linger time.Duration
	stop   <-chan struct{}
	spawn  func(func())
	// onClose, when set, is called once the connection is closed.
	onClose func(*conn)

	// wmu is held by Write while it writes, and by Close while it closes
	// the stream: the stream may not be closed while a Write runs.
	wmu sync.Mutex
	// dmu is held while a deadline is set, and by Close while it marks
	// the connection closed and sets its own: no deadline is set after.
	dmu    sync.Mutex
	closed atomic.Bool

	closeOnce sync.Once
	closeErr  error

	// now is closed by closeNow, and ends a wait for the peer.
	nowOnce   sync.Once
	now       chan struct{}
	closeNowO sync.Once
}

// nowChan returns now, made the first time.
func (c *conn) nowChan() chan struct{} {
	c.nowOnce.Do(func() { c.now = make(chan struct{}) })
	return c.now
}

// Write is the stream's. After Close it fails with net.ErrClosed.
func (c *conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed.Load() {
		return 0, net.ErrClosed
	}
	return c.stream.Write(p)
}

var _ net.Conn = (*conn)(nil)

func (c *conn) Read(p []byte) (int, error) { return c.stream.Read(p) }
func (c *conn) LocalAddr() net.Addr        { return c.local }
func (c *conn) RemoteAddr() net.Addr       { return c.remote }

// SetDeadline, SetReadDeadline and SetWriteDeadline are the stream's.
// Once Close has begun they do nothing and return net.ErrClosed: the
// deadline Close sets to wake a blocked Write must stay, and the SSH
// library sets the deadline at every Read and Write.
func (c *conn) SetDeadline(t time.Time) error {
	return c.setDeadline(c.stream.SetDeadline, t)
}

func (c *conn) SetReadDeadline(t time.Time) error {
	return c.setDeadline(c.stream.SetReadDeadline, t)
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	return c.setDeadline(c.stream.SetWriteDeadline, t)
}

func (c *conn) setDeadline(set func(time.Time) error, t time.Time) error {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	if c.closed.Load() {
		return net.ErrClosed
	}
	return set(t)
}

// Close closes the stream; a Read or a Write that is blocked returns at
// once. A connection of a Listener is closed after the peer has closed
// it or the linger has passed, in a goroutine, so that Close does not
// wait; one of DialLocal, and one whose listener is closing, is closed
// at once, the former with its endpoint. Only the first call of Close
// or closeNow does anything; every call returns its result.
func (c *conn) Close() error {
	return c.close(c.spawn != nil && !isClosed(c.stop))
}

// closeNow closes the connection at once, without a wait for the peer,
// and ends such a wait that has begun. The listener's Close closes the
// connections their owners have not closed with it.
func (c *conn) closeNow() error {
	c.closeNowO.Do(func() { close(c.nowChan()) })
	return c.close(false)
}

// close is Close, with the wait for the peer or without.
func (c *conn) close(linger bool) error {
	c.closeOnce.Do(func() {
		// A Write that is blocked returns at once with a timeout; what
		// Writes have handed the stream so far is still sent, and the
		// stream ends after it.
		c.dmu.Lock()
		c.closed.Store(true)
		c.stream.SetWriteDeadline(time.Unix(1, 0))
		c.dmu.Unlock()
		c.wmu.Lock()
		c.closeErr = c.stream.Close()
		c.wmu.Unlock()
		if c.onClose != nil {
			c.onClose(c)
		}
		if !linger {
			err := c.qc.CloseWithError(0, "closed")
			if c.ep != nil {
				err = errors.Join(err, c.ep.Shutdown(context.Background()))
			}
			c.closeErr = errors.Join(c.closeErr, err)
			return
		}
		c.spawn(c.lingerAndClose)
	})
	return c.closeErr
}

// lingerAndClose gives the peer the linger to close the connection, and
// then closes it. The listener's closing does not cut it short: the
// listener waits for it.
func (c *conn) lingerAndClose() {
	t := time.NewTimer(c.linger)
	defer t.Stop()
	select {
	case <-c.qc.Context().Done():
	case <-c.nowChan():
	case <-t.C:
	}
	c.qc.CloseWithError(0, "closed")
}
