package irohnet

// The mDNS announcer is the probe's peermdns.go (Task 0), made part of
// the package: an announcer that iroh-ssh, through the crate
// iroh-mdns-peer-lookup 0.1.2, can resolve.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"time"

	irohkey "github.com/tmc/go-iroh/key"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const (
	// mdnsService is the service name iroh-ssh asks for.
	mdnsService = "_irohv1._udp.local."
	// mdnsTTL is the TTL of the records announced, in seconds.
	mdnsTTL = 120
	// classFlush is class IN with the mDNS cache-flush bit: the record
	// set replaces what a listener has cached for the name.
	classFlush = dnsmessage.ClassINET | 1<<15

	// mdnsBurst is how often questions about the endpoint's own names
	// are answered: the resolver sends its four (SRV, TXT, A, AAAA) in
	// four packets within a few milliseconds, and one answer carries
	// every record.
	mdnsBurst = 50 * time.Millisecond
	// mdnsBrowseInterval is how often a browse of the whole service is
	// answered, as RFC 6762 section 6 asks of multicast responses.
	mdnsBrowseInterval = time.Second
)

// mdnsRepeats are the waits before the unsolicited announcements made
// when the announcer starts and whenever its addresses change.
var mdnsRepeats = []time.Duration{0, time.Second, 2 * time.Second}

var mdnsGroup4 = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// mdnsAnnouncer announces an endpoint on the local network over IPv4
// multicast, as iroh-ssh finds it, and answers the questions about it.
// Its answers go out from port 5353: the Rust side drops responses from
// any other source port.
type mdnsAnnouncer struct {
	id   irohkey.EndpointID
	log  *slog.Logger
	pc   net.PacketConn
	p    *ipv4.PacketConn
	kick chan struct{}

	// sendMu makes the choice of the interface and the write of one
	// packet one step.
	sendMu sync.Mutex

	mu sync.Mutex
	// joined are the interfaces the group was joined on, by index.
	joined map[int]net.Interface
	// announcement and goodbye are the packets for the current
	// addresses; nil while there is none.
	announcement, goodbye []byte
}

// listenMDNS binds the mDNS port, sharing it with other responders on
// the machine, and joins the group on every interface that is up,
// multicast, and neither a tunnel nor loopback. It fails when that cannot
// be had: the port taken without sharing, or no interface to join on.
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
	m := &mdnsAnnouncer{
		id:     id,
		log:    log,
		pc:     pc,
		p:      p,
		kick:   make(chan struct{}, 1),
		joined: make(map[int]net.Interface),
	}
	if m.join() == 0 {
		pc.Close()
		return nil, errors.New("mdns: no multicast interface")
	}
	return m, nil
}

// join joins the group on every fit interface it has not joined on yet,
// and returns on how many it is joined.
func (m *mdnsAnnouncer) join() int {
	all, _ := net.Interfaces()
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range all {
		ifc := all[i]
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagMulticast == 0 ||
			ifc.Flags&net.FlagPointToPoint != 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		if _, ok := m.joined[ifc.Index]; ok {
			continue
		}
		if err := m.p.JoinGroup(&ifc, mdnsGroup4); err == nil {
			m.joined[ifc.Index] = ifc
		}
	}
	return len(m.joined)
}

// setAddrs makes addrs what is announced, and announces them anew. With
// no address it says goodbye to what it announced before, and then
// announces nothing.
func (m *mdnsAnnouncer) setAddrs(addrs []netip.AddrPort) {
	var announcement, goodbye []byte
	if len(addrs) > 0 {
		var err error
		if announcement, err = mdnsPacket(m.id, addrs, mdnsTTL); err == nil {
			goodbye, err = mdnsPacket(m.id, addrs, 0)
		}
		if err != nil {
			m.log.Warn("mDNS: the announcement cannot be built", "error", err)
			announcement, goodbye = nil, nil
		}
	}
	m.mu.Lock()
	old := m.goodbye
	m.announcement, m.goodbye = announcement, goodbye
	m.mu.Unlock()
	if announcement == nil && old != nil {
		m.send(old)
	}
	m.join()
	select {
	case m.kick <- struct{}{}:
	default:
	}
}

// packets returns the current announcement and goodbye.
func (m *mdnsAnnouncer) packets() (announcement, goodbye []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.announcement, m.goodbye
}

// send multicasts packet on every interface the group was joined on.
func (m *mdnsAnnouncer) send(packet []byte) {
	if packet == nil {
		return
	}
	m.mu.Lock()
	ifaces := make([]net.Interface, 0, len(m.joined))
	for _, ifc := range m.joined {
		ifaces = append(ifaces, ifc)
	}
	m.mu.Unlock()
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	for i := range ifaces {
		if err := m.p.SetMulticastInterface(&ifaces[i]); err != nil {
			continue
		}
		_, _ = m.pc.WriteTo(packet, mdnsGroup4)
	}
}

// run announces and answers until ctx is done, then says goodbye and
// closes the socket.
func (m *mdnsAnnouncer) run(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.announce(ctx)
	}()
	m.answer(ctx)
	<-done
}

// announce makes the unsolicited announcements, at the start and after
// every change of the addresses, and at the end the goodbye.
func (m *mdnsAnnouncer) announce(ctx context.Context) {
	defer func() {
		_, goodbye := m.packets()
		m.send(goodbye)
		m.pc.Close()
	}()
	for {
		for i := 0; i < len(mdnsRepeats); {
			t := time.NewTimer(mdnsRepeats[i])
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-m.kick:
				t.Stop()
				i = 0
			case <-t.C:
				announcement, _ := m.packets()
				m.send(announcement)
				i++
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-m.kick:
		}
	}
}

// answer reads questions until the socket is closed, and answers those
// about the endpoint's names at most once per mdnsBurst, and a browse of
// the whole service at most once per mdnsBrowseInterval, with the whole
// record set.
func (m *mdnsAnnouncer) answer(ctx context.Context) {
	instance, host := mdnsNames(m.id)
	browse := strings.ToLower(mdnsService)
	own := []string{strings.ToLower(instance), strings.ToLower(host)}
	var lastSent time.Time
	buf := make([]byte, 9000)
	for {
		n, src, err := m.pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				m.log.Warn("mDNS no longer answers questions; it still announces", "error", err)
			}
			return
		}
		var parser dnsmessage.Parser
		hdr, err := parser.Start(buf[:n])
		if err != nil || hdr.Response {
			continue
		}
		questions, err := parser.AllQuestions()
		if err != nil {
			continue
		}
		wait := time.Duration(-1) // no question of ours
		for _, q := range questions {
			switch qname := strings.ToLower(q.Name.String()); {
			case qname == own[0] || qname == own[1]:
				wait = mdnsBurst
			case qname == browse && wait < 0:
				wait = mdnsBrowseInterval
			}
		}
		if wait < 0 || time.Since(lastSent) < wait {
			continue
		}
		announcement, _ := m.packets()
		if announcement == nil {
			continue
		}
		lastSent = time.Now()
		m.log.Debug("mDNS answering", "from", src, "question", questions[0].Name.String(), "type", questions[0].Type)
		m.send(announcement)
	}
}

// mdnsNames returns the instance and the host name of id: its z-base-32
// form, which is what iroh-ssh asks for.
func mdnsNames(id irohkey.EndpointID) (instance, host string) {
	z32 := id.Z32()
	return z32 + "." + mdnsService, z32 + ".local."
}

// mdnsPacket builds the response that carries the whole record set of id
// at addrs, with ttl; a ttl of 0 is the goodbye. Everything is in the
// answer section, so that it stands as an announcement as well as a reply
// to any one of the questions.
//
// What iroh-ssh's resolver (hick's resolve_instance) asks for and needs:
//
//	SRV  <z32 id>._irohv1._udp.local.  -> port, target <z32 id>.local.
//	TXT  <z32 id>._irohv1._udp.local.  -> must be present, even if empty
//	A    <z32 id>.local.               (AAAA too)
//
// The dialable addresses are (every A/AAAA of the target) x (the SRV
// port), plus one address per TXT segment "a<N>=" followed by the 4 or 16
// address bytes and the port, big endian. The lookup completes at the
// first moment it holds SRV, TXT and one address, so only the TXT
// segments are sure to arrive together: the first address goes in A or
// AAAA, every further one in TXT, as the Rust announcer does.
//
// go-iroh's iroh/mdns differs in two ways that each make the lookup
// fail: its names are the ID in RFC 4648 base32, and it sends no TXT
// record unless there is a relay URL or user data.
func mdnsPacket(id irohkey.EndpointID, addrs []netip.AddrPort, ttl uint32) ([]byte, error) {
	if len(addrs) == 0 {
		return nil, errors.New("mdns: no address to announce")
	}
	instance, host := mdnsNames(id)
	svcName, err := dnsmessage.NewName(mdnsService)
	if err != nil {
		return nil, err
	}
	instName, err := dnsmessage.NewName(instance)
	if err != nil {
		return nil, err
	}
	hostName, err := dnsmessage.NewName(host)
	if err != nil {
		return nil, err
	}
	first := addrs[0]

	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	b.EnableCompression()
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	// PTR is a shared record: no cache-flush bit.
	if err := b.PTRResource(
		dnsmessage.ResourceHeader{Name: svcName, Class: dnsmessage.ClassINET, TTL: ttl},
		dnsmessage.PTRResource{PTR: instName},
	); err != nil {
		return nil, err
	}
	if err := b.SRVResource(
		dnsmessage.ResourceHeader{Name: instName, Class: classFlush, TTL: ttl},
		dnsmessage.SRVResource{Port: first.Port(), Target: hostName},
	); err != nil {
		return nil, err
	}
	// TXT: every address after the first. With none, one empty string,
	// which DNS-SD requires anyway and without which the resolver never
	// completes.
	var txt []string
	for i, ap := range addrs[1:] {
		seg := append([]byte(fmt.Sprintf("a%d=", i+1)), ap.Addr().Unmap().AsSlice()...)
		seg = binary.BigEndian.AppendUint16(seg, ap.Port())
		txt = append(txt, string(seg))
	}
	if len(txt) == 0 {
		txt = []string{""}
	}
	if err := b.TXTResource(
		dnsmessage.ResourceHeader{Name: instName, Class: classFlush, TTL: ttl},
		dnsmessage.TXTResource{TXT: txt},
	); err != nil {
		return nil, err
	}
	hdr := dnsmessage.ResourceHeader{Name: hostName, Class: classFlush, TTL: ttl}
	if ip := first.Addr().Unmap(); ip.Is4() {
		err = b.AResource(hdr, dnsmessage.AResource{A: ip.As4()})
	} else {
		err = b.AAAAResource(hdr, dnsmessage.AAAAResource{AAAA: ip.As16()})
	}
	if err != nil {
		return nil, err
	}
	return b.Finish()
}
