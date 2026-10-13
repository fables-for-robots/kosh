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
	// linger is how long Close waits, between closing the stream and
	// closing the connection, for the peer to close the connection:
	// closing it at once would drop what was written and not yet
	// acknowledged. Zero closes at once. stop ends the wait early.
	linger time.Duration
	stop   <-chan struct{}
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

// Close closes the stream, then the iroh connection, then the endpoint
// of a connection DialLocal made. A Read or Write that is blocked
// returns at once. Between the stream and the connection it waits for
// the peer to close the connection, linger at most, so that what was
// written last reaches it. Only the first call does anything; every
// call returns its result.
func (c *conn) Close() error {
	c.closeOnce.Do(func() {
		err := c.stream.Close()
		if c.linger > 0 {
			t := time.NewTimer(c.linger)
			select {
			case <-c.qc.Context().Done():
			case <-c.stop:
			case <-t.C:
			}
			t.Stop()
		}
		err = errors.Join(err, c.qc.CloseWithError(0, "closed"))
		if c.ep != nil {
			err = errors.Join(err, c.ep.Shutdown(context.Background()))
		}
		c.closeErr = err
		if c.onClose != nil {
			c.onClose(c)
		}
	})
	return c.closeErr
}
