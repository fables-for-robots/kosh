package irohnet

import (
	"context"
	"errors"
	"net"
	"sync"
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
	// yet acknowledged. stop ends the wait early. spawn runs the wait
	// in a goroutine its owner counts. Without spawn the connection is
	// closed at once.
	linger time.Duration
	stop   <-chan struct{}
	spawn  func(func())
	// onClose, when set, is called once the connection is closed.
	onClose func(*conn)

	closeOnce sync.Once
	closeErr  error
}

var _ net.Conn = (*conn)(nil)

func (c *conn) Read(p []byte) (int, error)         { return c.stream.Read(p) }
func (c *conn) Write(p []byte) (int, error)        { return c.stream.Write(p) }
func (c *conn) LocalAddr() net.Addr                { return c.local }
func (c *conn) RemoteAddr() net.Addr               { return c.remote }
func (c *conn) SetDeadline(t time.Time) error      { return c.stream.SetDeadline(t) }
func (c *conn) SetReadDeadline(t time.Time) error  { return c.stream.SetReadDeadline(t) }
func (c *conn) SetWriteDeadline(t time.Time) error { return c.stream.SetWriteDeadline(t) }

// Close closes the stream; a Read or Write that is blocked returns at
// once. A connection of a Listener is closed after the peer has closed
// it or the linger has passed, in a goroutine, so that Close does not
// wait; one of DialLocal is closed at once, with its endpoint. Only the
// first call does anything; every call returns its result.
func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.stream.Close()
		if c.onClose != nil {
			c.onClose(c)
		}
		if c.spawn == nil {
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
// then closes it.
func (c *conn) lingerAndClose() {
	t := time.NewTimer(c.linger)
	defer t.Stop()
	select {
	case <-c.qc.Context().Done():
	case <-c.stop:
	case <-t.C:
	}
	c.qc.CloseWithError(0, "closed")
}
