package irohnet

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log/slog"
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
