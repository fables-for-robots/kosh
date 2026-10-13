package irohnet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// parsedRecords are the records of an announcement, parsed back.
type parsedRecords struct {
	ptr   []string
	srv   []dnsmessage.SRVResource
	txt   [][]string
	addrs []netip.Addr
	names map[dnsmessage.Type]string
	ttls  []uint32
	flush map[dnsmessage.Type]bool
}

func parseAnnouncement(t *testing.T, packet []byte) parsedRecords {
	t.Helper()
	var p dnsmessage.Parser
	hdr, err := p.Start(packet)
	if err != nil {
		t.Fatal(err)
	}
	if !hdr.Response || !hdr.Authoritative {
		t.Fatalf("header %+v: want an authoritative response", hdr)
	}
	if err := p.SkipAllQuestions(); err != nil {
		t.Fatal(err)
	}
	answers, err := p.AllAnswers()
	if err != nil {
		t.Fatal(err)
	}
	r := parsedRecords{names: map[dnsmessage.Type]string{}, flush: map[dnsmessage.Type]bool{}}
	for _, a := range answers {
		r.names[a.Header.Type] = a.Header.Name.String()
		r.ttls = append(r.ttls, a.Header.TTL)
		r.flush[a.Header.Type] = a.Header.Class&(1<<15) != 0
		if a.Header.Class&^(1<<15) != dnsmessage.ClassINET {
			t.Errorf("%v record of class %v", a.Header.Type, a.Header.Class)
		}
		switch b := a.Body.(type) {
		case *dnsmessage.PTRResource:
			r.ptr = append(r.ptr, b.PTR.String())
		case *dnsmessage.SRVResource:
			r.srv = append(r.srv, *b)
		case *dnsmessage.TXTResource:
			r.txt = append(r.txt, b.TXT)
		case *dnsmessage.AResource:
			r.addrs = append(r.addrs, netip.AddrFrom4(b.A))
		case *dnsmessage.AAAAResource:
			r.addrs = append(r.addrs, netip.AddrFrom16(b.AAAA))
		default:
			t.Errorf("unexpected record %v", a.Header.Type)
		}
	}
	return r
}

// txtAddr decodes a TXT segment "a<N>=" + address bytes + port.
func txtAddr(t *testing.T, n int, seg string) netip.AddrPort {
	t.Helper()
	prefix := fmt.Sprintf("a%d=", n)
	if len(seg) < len(prefix) || seg[:len(prefix)] != prefix {
		t.Fatalf("TXT segment %q does not begin with %q", seg, prefix)
	}
	b := []byte(seg[len(prefix):])
	if len(b) != 4+2 && len(b) != 16+2 {
		t.Fatalf("TXT segment %q: %d bytes after the key", seg, len(b))
	}
	ip, _ := netip.AddrFromSlice(b[:len(b)-2])
	return netip.AddrPortFrom(ip, binary.BigEndian.Uint16(b[len(b)-2:]))
}

func TestMDNSPacket(t *testing.T) {
	id := newKey(t).Public().EndpointID()
	z32 := id.Z32()
	instance := z32 + "._irohv1._udp.local."
	host := z32 + ".local."

	for _, tc := range []struct {
		name  string
		addrs []netip.AddrPort
	}{
		{"one address", addrPorts("192.168.1.50:4435")},
		{"three addresses", addrPorts("[2001:db8::1234]:4435", "192.168.1.50:4435", "10.241.247.228:4435")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, ttl := range []uint32{mdnsTTL, 0} {
				packet, err := mdnsPacket(id, tc.addrs, ttl)
				if err != nil {
					t.Fatal(err)
				}
				r := parseAnnouncement(t, packet)

				if r.names[dnsmessage.TypePTR] != "_irohv1._udp.local." || !slices.Equal(r.ptr, []string{instance}) {
					t.Errorf("PTR %s -> %v, want _irohv1._udp.local. -> %s", r.names[dnsmessage.TypePTR], r.ptr, instance)
				}
				if r.flush[dnsmessage.TypePTR] {
					t.Error("the PTR record, a shared one, has the cache-flush bit")
				}
				if len(r.srv) != 1 || r.names[dnsmessage.TypeSRV] != instance ||
					r.srv[0].Port != tc.addrs[0].Port() || r.srv[0].Target.String() != host {
					t.Errorf("SRV %s %+v, want %s port %d target %s", r.names[dnsmessage.TypeSRV], r.srv, instance, tc.addrs[0].Port(), host)
				}
				if len(r.txt) != 1 || r.names[dnsmessage.TypeTXT] != instance {
					t.Fatalf("TXT %s %q: want one TXT record for %s", r.names[dnsmessage.TypeTXT], r.txt, instance)
				}
				if len(tc.addrs) == 1 {
					if !slices.Equal(r.txt[0], []string{""}) {
						t.Errorf("TXT %q with one address, want one empty string", r.txt[0])
					}
				} else {
					if len(r.txt[0]) != len(tc.addrs)-1 {
						t.Fatalf("TXT %q: want %d segments", r.txt[0], len(tc.addrs)-1)
					}
					for i, seg := range r.txt[0] {
						if got := txtAddr(t, i+1, seg); got != tc.addrs[i+1] {
							t.Errorf("TXT segment %d is %v, want %v", i+1, got, tc.addrs[i+1])
						}
					}
				}
				addrName := r.names[dnsmessage.TypeA] + r.names[dnsmessage.TypeAAAA]
				if !slices.Equal(r.addrs, []netip.Addr{tc.addrs[0].Addr()}) || addrName != host {
					t.Errorf("A/AAAA %s %v, want %s %v", addrName, r.addrs, host, tc.addrs[0].Addr())
				}
				for _, typ := range []dnsmessage.Type{dnsmessage.TypeSRV, dnsmessage.TypeTXT} {
					if !r.flush[typ] {
						t.Errorf("%v has no cache-flush bit", typ)
					}
				}
				for _, got := range r.ttls {
					if got != ttl {
						t.Errorf("TTL %d, want %d", got, ttl)
					}
				}
			}
		})
	}

	if _, err := mdnsPacket(id, nil, mdnsTTL); err == nil {
		t.Error("a packet without an address was built")
	}
}

// question builds an mDNS query for name and type, or a response when
// response is set.
func question(t *testing.T, name string, typ dnsmessage.Type, response bool) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: response})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	packet, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

func TestMDNSAnswersWhatItShould(t *testing.T) {
	id := newKey(t).Public().EndpointID()
	instance, host := mdnsNames(id)
	group := netip.MustParseAddr("224.0.0.251")
	now := time.Now()
	long := now.Add(-time.Hour)
	other := newKey(t).Public().EndpointID()
	otherInstance, _ := mdnsNames(other)

	for _, tc := range []struct {
		name     string
		packet   []byte
		dst      netip.Addr
		lastSent time.Time
		want     bool
	}{
		{"its instance", question(t, instance, dnsmessage.TypeSRV, false), group, long, true},
		{"its host", question(t, host, dnsmessage.TypeA, false), group, long, true},
		{"its host in capitals", question(t, strings.ToUpper(host[:10])+host[10:], dnsmessage.TypeA, false), group, long, true},
		{"its instance, within the burst", question(t, instance, dnsmessage.TypeTXT, false), group, now.Add(-mdnsBurst / 2), false},
		{"its instance, after the burst", question(t, instance, dnsmessage.TypeTXT, false), group, now.Add(-mdnsBurst), true},
		{"a browse", question(t, mdnsService, dnsmessage.TypePTR, false), group, long, true},
		{"a browse, within the second", question(t, mdnsService, dnsmessage.TypePTR, false), group, now.Add(-mdnsBrowseInterval / 2), false},
		{"a browse, after the second", question(t, mdnsService, dnsmessage.TypePTR, false), group, now.Add(-mdnsBrowseInterval), true},
		{"another endpoint", question(t, otherInstance, dnsmessage.TypeSRV, false), group, long, false},
		{"another service", question(t, "_http._tcp.local.", dnsmessage.TypePTR, false), group, long, false},
		{"a response", question(t, instance, dnsmessage.TypeSRV, true), group, long, false},
		{"malformed", []byte{0x00, 0x01, 0x02}, group, long, false},
		{"truncated question", question(t, instance, dnsmessage.TypeSRV, false)[:20], group, long, false},
		{"unicast", question(t, instance, dnsmessage.TypeSRV, false), netip.MustParseAddr("192.168.1.92"), long, false},
		{"no destination", question(t, instance, dnsmessage.TypeSRV, false), netip.Addr{}, long, false},
	} {
		got, why := mdnsShouldAnswer(tc.packet, tc.dst, now, tc.lastSent, instance, host)
		if got != tc.want {
			t.Errorf("%s: answer %v (%s), want %v", tc.name, got, why, tc.want)
		}
		if !got && why == "" {
			t.Errorf("%s: not answered, and no reason given", tc.name)
		}
	}
}

func TestMDNSSetAddrs(t *testing.T) {
	id := newKey(t).Public().EndpointID()
	var sent [][]byte
	m := &mdnsAnnouncer{
		id:   id,
		log:  slog.New(slog.DiscardHandler),
		kick: make(chan struct{}, 1),
		send: func(p []byte) { sent = append(sent, p) },
	}
	kicked := func() bool {
		select {
		case <-m.kick:
			return true
		default:
			return false
		}
	}
	a := addrPorts("192.168.1.50:4435", "10.0.0.2:4435")
	b := addrPorts("192.168.1.50:4435")

	m.setAddrs(a)
	announcement, goodbye := m.packets()
	wantA, _ := mdnsPacket(id, a, mdnsTTL)
	byeA, _ := mdnsPacket(id, a, 0)
	if !kicked() || !bytes.Equal(announcement, wantA) || !bytes.Equal(goodbye, byeA) || len(sent) != 0 {
		t.Fatalf("first addresses: kicked, announcement and goodbye of them, nothing sent yet; sent %d", len(sent))
	}

	m.setAddrs(slices.Clone(a))
	if kicked() || len(sent) != 0 {
		t.Fatal("the same addresses again announced anew")
	}

	m.setAddrs(b)
	announcement, _ = m.packets()
	wantB, _ := mdnsPacket(id, b, mdnsTTL)
	if !kicked() || !bytes.Equal(announcement, wantB) || len(sent) != 0 {
		t.Fatal("changed addresses are not announced anew")
	}

	m.setAddrs(nil)
	announcement, goodbye = m.packets()
	byeB, _ := mdnsPacket(id, b, 0)
	if announcement != nil || goodbye != nil || len(sent) != 1 || !bytes.Equal(sent[0], byeB) {
		t.Fatalf("no addresses: want the goodbye of the last ones sent and nothing announced; sent %d", len(sent))
	}
}

// fakeGroup stands for the socket's group memberships: it records the
// joins and the leaves, and refuses a join beyond limit memberships as
// the kernel does beyond its own (20 on Linux).
type fakeGroup struct {
	limit  int
	in     map[int]string
	events []string
}

func (g *fakeGroup) JoinGroup(ifc *net.Interface, group net.Addr) error {
	if group.String() != "224.0.0.251:5353" {
		return fmt.Errorf("joined %v", group)
	}
	if _, ok := g.in[ifc.Index]; ok {
		return errors.New("address already in use")
	}
	if g.limit > 0 && len(g.in) >= g.limit {
		g.events = append(g.events, "join "+ifc.Name+" refused")
		return errors.New("no buffer space available")
	}
	if g.in == nil {
		g.in = map[int]string{}
	}
	g.in[ifc.Index] = ifc.Name
	g.events = append(g.events, "join "+ifc.Name)
	return nil
}

func (g *fakeGroup) LeaveGroup(ifc *net.Interface, _ net.Addr) error {
	if _, ok := g.in[ifc.Index]; !ok {
		return errors.New("cannot assign requested address")
	}
	delete(g.in, ifc.Index)
	g.events = append(g.events, "leave "+ifc.Name)
	return nil
}

// take returns what happened since it was last called.
func (g *fakeGroup) take() []string {
	ev := g.events
	g.events = nil
	return ev
}

// testAnnouncer is an announcer without a socket: its memberships are
// g's, and what it sends on an interface is recorded in g's events as
// "send <interface> <what>", where what is "announcement" or "goodbye".
func testAnnouncer(t *testing.T, g *fakeGroup) (*mdnsAnnouncer, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	m := &mdnsAnnouncer{
		id:     newKey(t).Public().EndpointID(),
		log:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		kick:   make(chan struct{}, 1),
		grp:    g,
		joined: make(map[int]net.Interface),
	}
	m.send = m.multicast
	m.sendOn = func(ifc *net.Interface, packet []byte) {
		what := "announcement"
		if _, goodbye := m.packets(); bytes.Equal(packet, goodbye) {
			what = "goodbye"
		}
		g.events = append(g.events, "send "+ifc.Name+" "+what)
	}
	return m, logs
}

func netIfaces(names ...string) []net.Interface {
	var out []net.Interface
	for _, name := range names {
		var index int
		fmt.Sscanf(name, "if%d", &index)
		out = append(out, net.Interface{Index: index, Name: name, Flags: lan})
	}
	return out
}

func drained(m *mdnsAnnouncer) bool {
	select {
	case <-m.kick:
		return true
	default:
		return false
	}
}

// The group is joined on the interfaces the announcer is given, an
// interface that comes is joined and announced on, and one that goes
// hears the goodbye and is left. What is sent goes to the joined ones.
func TestMDNSJoinsWhereItAnnounces(t *testing.T) {
	g := &fakeGroup{}
	m, _ := testAnnouncer(t, g)

	m.setInterfaces(netIfaces("if1"))
	if got := g.take(); !slices.Equal(got, []string{"join if1"}) {
		t.Fatalf("first interfaces: %v, want a join on if1", got)
	}
	m.setAddrs(addrPorts("192.168.1.50:4435"))
	drained(m)
	announcement, _ := m.packets()
	m.send(announcement)
	if got := g.take(); !slices.Equal(got, []string{"send if1 announcement"}) {
		t.Fatalf("an announcement: %v, want it sent on if1 alone", got)
	}

	// The same again: nothing happens.
	m.setInterfaces(netIfaces("if1"))
	if got := g.take(); len(got) != 0 || drained(m) {
		t.Fatalf("the same interfaces again: %v", got)
	}

	// if2 gained an address: joined, and announced anew.
	m.setInterfaces(netIfaces("if1", "if2"))
	if got := g.take(); !slices.Equal(got, []string{"join if2"}) || !drained(m) {
		t.Fatalf("a second interface: %v, want a join on if2 and the announcement made anew", got)
	}
	m.send(announcement)
	got := g.take()
	slices.Sort(got)
	if !slices.Equal(got, []string{"send if1 announcement", "send if2 announcement"}) {
		t.Fatalf("an announcement: %v, want it sent on if1 and if2", got)
	}

	// if1 lost its last announced address: it hears the goodbye of what
	// was announced there, and is left.
	m.setInterfaces(netIfaces("if2"))
	if got := g.take(); !slices.Equal(got, []string{"send if1 goodbye", "leave if1"}) {
		t.Fatalf("an interface gone: %v, want the goodbye on if1 and then the leave", got)
	}
	m.send(announcement)
	if got := g.take(); !slices.Equal(got, []string{"send if2 announcement"}) {
		t.Fatalf("an announcement: %v, want it sent on if2 alone", got)
	}

	// Questions are answered where the group is joined, and where the
	// interface of a packet is not known.
	for index, want := range map[int]bool{2: true, 1: false, 7: false, 0: true} {
		if got := m.answersOn(index); got != want {
			t.Errorf("a question that arrived on interface %d: answered %v, want %v", index, got, want)
		}
	}

	// No interface at all: all are left, with nothing to say goodbye
	// with once no address is announced.
	m.setAddrs(nil)
	g.take()
	m.setInterfaces(nil)
	if got := g.take(); !slices.Equal(got, []string{"leave if2"}) {
		t.Fatalf("no interface: %v, want the leave of if2", got)
	}
	m.send(announcement)
	if got := g.take(); len(got) != 0 {
		t.Fatalf("an announcement without an interface: %v", got)
	}
}

// More interfaces than the kernel lets one socket join on: every join
// that fails is said at warn level with the interface's name, once, and
// tried again at every reading, so that it holds once there is room.
func TestMDNSJoinErrorsAreLogged(t *testing.T) {
	g := &fakeGroup{limit: 20}
	m, logs := testAnnouncer(t, g)
	var names []string
	for i := 1; i <= 25; i++ {
		names = append(names, fmt.Sprintf("if%d", i))
	}
	warnings := func(name string) int {
		n := 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "level=WARN") && strings.Contains(line, "interface="+name+" ") {
				n++
			}
		}
		return n
	}

	for reading := 1; reading <= 3; reading++ {
		m.setInterfaces(netIfaces(names...))
		if len(g.in) != 20 {
			t.Fatalf("reading %d: joined on %d interfaces, want 20", reading, len(g.in))
		}
		for i, name := range names {
			want := 0
			if i >= 20 {
				want = 1
			}
			if got := warnings(name); got != want {
				t.Errorf("reading %d: %d warnings name %s, want %d", reading, got, name, want)
			}
		}
	}
	if !strings.Contains(logs.String(), "no buffer space available") {
		t.Errorf("the warnings do not say why the join failed:\n%s", logs)
	}
	refused := 0
	for _, ev := range g.take() {
		if strings.HasSuffix(ev, " refused") {
			refused++
		}
	}
	if refused != 3*5 {
		t.Errorf("%d joins were refused in three readings, want each of the 5 tried at each", refused)
	}

	// Ten interfaces lose their addresses: the five that waited are
	// joined, and say nothing more.
	m.setInterfaces(netIfaces(names[10:]...))
	if len(g.in) != 15 {
		t.Fatalf("joined on %d interfaces, want the 15 that are left", len(g.in))
	}
	for _, name := range names[20:] {
		if got := warnings(name); got != 1 {
			t.Errorf("%d warnings name %s, want the one", got, name)
		}
	}
	// One that failed, went away and fails again is said again.
	g.limit = 15
	m.setInterfaces(netIfaces(names[10:]...))
	m.setInterfaces(netIfaces(names[9:]...))
	m.setInterfaces(netIfaces(names[9:]...))
	if got := warnings("if10"); got != 1 {
		t.Errorf("%d warnings name if10, want 1", got)
	}
	m.setInterfaces(netIfaces(names[10:]...))
	m.setInterfaces(netIfaces(names[9:]...))
	if got := warnings("if10"); got != 2 {
		t.Errorf("%d warnings name if10 after it went and came back, want 2", got)
	}
}
