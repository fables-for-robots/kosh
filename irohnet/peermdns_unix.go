//go:build unix && !solaris

package irohnet

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"syscall"

	irohkey "github.com/tmc/go-iroh/key"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// listenMDNS binds the mDNS port, sharing it with other responders on
// the machine. The group it joins later, on the interfaces setInterfaces
// is given. It fails when the port cannot be had, taken without sharing,
// or there is no way to read where a packet was sent.
func listenMDNS(ctx context.Context, id irohkey.EndpointID, log *slog.Logger) (*mdnsAnnouncer, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); serr != nil {
				return
			}
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", "0.0.0.0:5353")
	if err != nil {
		return nil, fmt.Errorf("mdns: listen: %w", err)
	}
	p := ipv4.NewPacketConn(pc)
	_ = p.SetMulticastLoopback(true) // clients on this machine must hear it
	_ = p.SetMulticastTTL(255)
	// Where a packet was sent, and on which interface it arrived: only
	// questions to the group are answered, and only where it is joined.
	if err := p.SetControlMessage(ipv4.FlagDst|ipv4.FlagInterface, true); err != nil {
		pc.Close()
		return nil, fmt.Errorf("mdns: the destination of packets cannot be read: %w", err)
	}
	m := &mdnsAnnouncer{
		id:     id,
		log:    log,
		pc:     pc,
		p:      p,
		kick:   make(chan struct{}, 1),
		grp:    p,
		joined: make(map[int]net.Interface),
	}
	m.send = m.multicast
	m.sendOn = m.writeOn
	return m, nil
}
