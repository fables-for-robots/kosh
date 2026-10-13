package irohnet

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/tmc/go-iroh/iroh"
	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/netaddr"
	"github.com/tmc/go-iroh/relay"
)

// DialLocal connects to the endpoint with the given ID at addrs, without
// a relay and without looking anything up, opens the stream and returns
// it. It is for tests: kosh has no client. The caller writes first, as
// an SSH client does; the listener sees the stream only then.
func DialLocal(ctx context.Context, id string, addrs []netip.AddrPort) (net.Conn, error) {
	remote, err := irohkey.ParseEndpointID(id)
	if err != nil {
		return nil, fmt.Errorf("irohnet: dial: endpoint ID %q: %w", id, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("irohnet: dial %s: no address", id)
	}
	sk, err := irohkey.GenerateSecretKey()
	if err != nil {
		return nil, fmt.Errorf("irohnet: dial %s: %w", id, err)
	}
	ep, err := iroh.Bind(ctx,
		iroh.WithSecretKey(sk),
		iroh.WithRelayMode(relay.ModeDisabled()),
		iroh.WithBindAddr(netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 0)))
	if err != nil {
		return nil, fmt.Errorf("irohnet: dial %s: bind: %w", id, err)
	}

	targets := make([]netaddr.TransportAddr, len(addrs))
	for i, ap := range addrs {
		targets[i] = netaddr.IPAddr{Addr: ap}
	}
	qc, err := ep.Connect(ctx, netaddr.NewEndpointAddr(remote, targets...), ALPN)
	if err != nil {
		ep.Shutdown(context.Background())
		return nil, fmt.Errorf("irohnet: dial %s: %w", id, err)
	}
	stream, err := qc.OpenStreamConn(ctx)
	if err != nil {
		err = errors.Join(err, qc.Close(), ep.Shutdown(context.Background()))
		return nil, fmt.Errorf("irohnet: dial %s: open stream: %w", id, err)
	}
	return &conn{
		stream: stream,
		qc:     qc,
		ep:     ep,
		local:  Addr{ID: ep.ID().String()},
		remote: Addr{ID: remote.String()},
	}, nil
}
