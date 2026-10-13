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
	"slices"
	"strings"
	"sync"
	"time"

	irohkey "github.com/tmc/go-iroh/key"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
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

// mdnsGroup4 is where announcements and answers are sent.
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
	// grp is where the group is joined and left: p, unless a test says
	// otherwise.
	grp groupMember
	// send multicasts a packet on every interface the group is joined
	// on: multicast, unless a test says otherwise.
	send func(packet []byte)
	// sendOn multicasts a packet on one interface: writeOn, unless a
	// test says otherwise.
	sendOn func(ifc *net.Interface, packet []byte)

	// sendMu makes the choice of the interface and the write of one
	// packet one step.
	sendMu sync.Mutex

	// ifMu is held while the memberships are made what setInterfaces was
	// given: one call at a time.
	ifMu sync.Mutex
	// joinFailed are the interfaces on which the join failed and was
	// said to have, by index, so that it is said once; guarded by ifMu.
	joinFailed map[int]bool

	mu sync.Mutex
	// joined are the interfaces the group is joined on, by index.
	joined map[int]net.Interface
	// addrs are the addresses announced, and announcement and goodbye
	// their packets; nil while there is none.
	addrs                 []netip.AddrPort
	announcement, goodbye []byte
}

// groupMember is what joins and leaves a multicast group on an
// interface: an ipv4.PacketConn.
type groupMember interface {
	JoinGroup(ifi *net.Interface, group net.Addr) error
	LeaveGroup(ifi *net.Interface, group net.Addr) error
}

// setInterfaces makes on the interfaces the group is joined on: those
// that have an address that is announced. An interface that is no longer
// among them is sent the goodbye of what is announced, so that who
// listens there forgets it, and is left. One that is new is joined, and
// the announcement is then made anew. A join that fails (the kernel
// allows one socket 20 memberships on Linux) is a warning that names the
// interface, once for as long as the interface is among them, and is
// tried again at every call.
func (m *mdnsAnnouncer) setInterfaces(on []net.Interface) {
	m.ifMu.Lock()
	defer m.ifMu.Unlock()

	want := make(map[int]bool, len(on))
	for _, ifc := range on {
		want[ifc.Index] = true
	}
	m.mu.Lock()
	var leave []net.Interface
	for index, ifc := range m.joined {
		if !want[index] {
			leave = append(leave, ifc)
		}
	}
	var join []net.Interface
	for _, ifc := range on {
		if _, ok := m.joined[ifc.Index]; !ok {
			join = append(join, ifc)
		}
	}
	goodbye := m.goodbye
	m.mu.Unlock()

	slices.SortFunc(leave, func(a, b net.Interface) int { return a.Index - b.Index })
	for i := range leave {
		ifc := &leave[i]
		if goodbye != nil {
			m.sendOn(ifc, goodbye)
		}
		// Whatever the kernel says: the interface may be gone.
		if err := m.grp.LeaveGroup(ifc, mdnsGroup4); err != nil {
			m.log.Debug("mDNS: leaving the group", "interface", ifc.Name, "error", err)
		}
		m.mu.Lock()
		delete(m.joined, ifc.Index)
		m.mu.Unlock()
	}
	for index := range m.joinFailed {
		if !want[index] {
			delete(m.joinFailed, index)
		}
	}

	joined := false
	for i := range join {
		ifc := &join[i]
		if err := m.grp.JoinGroup(ifc, mdnsGroup4); err != nil {
			if !m.joinFailed[ifc.Index] {
				if m.joinFailed == nil {
					m.joinFailed = make(map[int]bool)
				}
				m.joinFailed[ifc.Index] = true
				m.log.Warn("mDNS cannot join the group on an interface, and is not announced there; it tries again when it reads the addresses",
					"interface", ifc.Name, "error", err)
			}
			continue
		}
		delete(m.joinFailed, ifc.Index)
		m.mu.Lock()
		m.joined[ifc.Index] = *ifc
		m.mu.Unlock()
		joined = true
	}
	if joined {
		select {
		case m.kick <- struct{}{}:
		default:
		}
	}
}

// answersOn reports whether a question that arrived on the interface
// with index is answered: the group is joined there. The kernel hands
// the socket questions from other interfaces too, when another program
// on the machine joined the group on them. A packet whose interface is
// not known (index 0) is answered.
func (m *mdnsAnnouncer) answersOn(index int) bool {
	if index == 0 {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.joined[index]
	return ok
}

// setAddrs makes addrs what is announced, and announces them anew when
// they differ from what was announced. With no address it says goodbye
// to what it announced before, and then announces nothing.
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
	if slices.Equal(m.addrs, addrs) && m.addrs != nil {
		m.mu.Unlock()
		return
	}
	old := m.goodbye
	m.addrs = slices.Clone(addrs)
	m.announcement, m.goodbye = announcement, goodbye
	m.mu.Unlock()
	if announcement == nil && old != nil {
		m.send(old)
	}
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

// multicast sends packet on every interface the group is joined on.
func (m *mdnsAnnouncer) multicast(packet []byte) {
	if packet == nil {
		return
	}
	m.mu.Lock()
	ifaces := make([]net.Interface, 0, len(m.joined))
	for _, ifc := range m.joined {
		ifaces = append(ifaces, ifc)
	}
	m.mu.Unlock()
	for i := range ifaces {
		m.sendOn(&ifaces[i], packet)
	}
}

// writeOn sends packet to the group on ifc.
func (m *mdnsAnnouncer) writeOn(ifc *net.Interface, packet []byte) {
	m.sendMu.Lock()
	defer m.sendMu.Unlock()
	if err := m.p.SetMulticastInterface(ifc); err != nil {
		return
	}
	_, _ = m.pc.WriteTo(packet, mdnsGroup4)
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

// answer reads packets until the socket is closed, and answers those
// mdnsShouldAnswer says to answer with the whole record set.
func (m *mdnsAnnouncer) answer(ctx context.Context) {
	instance, host := mdnsNames(m.id)
	var lastSent time.Time
	buf := make([]byte, 9000)
	for {
		n, cm, src, err := m.p.ReadFrom(buf)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, net.ErrClosed) {
				m.log.Warn("mDNS no longer answers questions; it still announces", "error", err)
			}
			return
		}
		var dst netip.Addr
		index := 0
		if cm != nil {
			dst, _ = netip.AddrFromSlice(cm.Dst)
			dst = dst.Unmap()
			index = cm.IfIndex
		}
		if !m.answersOn(index) {
			continue
		}
		now := time.Now()
		if ok, _ := mdnsShouldAnswer(buf[:n], dst, now, lastSent, instance, host); !ok {
			continue
		}
		announcement, _ := m.packets()
		if announcement == nil {
			continue
		}
		lastSent = now
		m.log.Debug("mDNS answering", "from", src)
		m.send(announcement)
	}
}

// mdnsGroup is the address of the mDNS group.
var mdnsGroup = netip.MustParseAddr("224.0.0.251")

// mdnsShouldAnswer decides whether the answering loop answers packet,
// which arrived for dst at now, when it last answered at lastSent, and
// says why not. It answers only a query sent to the mDNS group (not one
// sent to the machine's address, which anyone who reaches its port 5353
// could send), with a question about instance or host, at most once per
// mdnsBurst, or a browse of the whole service, at most once per
// mdnsBrowseInterval. A response, or what cannot be parsed, is never
// answered.
func mdnsShouldAnswer(packet []byte, dst netip.Addr, now, lastSent time.Time, instance, host string) (bool, string) {
	if dst != mdnsGroup {
		return false, "not sent to the mDNS group"
	}
	var parser dnsmessage.Parser
	hdr, err := parser.Start(packet)
	if err != nil {
		return false, "malformed header"
	}
	if hdr.Response {
		return false, "a response"
	}
	questions, err := parser.AllQuestions()
	if err != nil {
		return false, "malformed questions"
	}
	wait := time.Duration(-1) // no question of ours
	for _, q := range questions {
		switch qname := q.Name.String(); {
		case strings.EqualFold(qname, instance) || strings.EqualFold(qname, host):
			wait = mdnsBurst
		case strings.EqualFold(qname, mdnsService) && wait < 0:
			wait = mdnsBrowseInterval
		}
	}
	if wait < 0 {
		return false, "no question about this endpoint"
	}
	if now.Sub(lastSent) < wait {
		return false, "answered a moment ago"
	}
	return true, ""
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
