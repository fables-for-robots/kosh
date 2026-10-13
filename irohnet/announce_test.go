package irohnet

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tmc/go-iroh/netaddr"
)

func ipNet(s string) *net.IPNet {
	ip, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}

const (
	lan    = net.FlagUp | net.FlagBroadcast | net.FlagMulticast
	tunnel = net.FlagUp | net.FlagPointToPoint | net.FlagMulticast
	loop   = net.FlagUp | net.FlagLoopback | net.FlagMulticast
)

// madeUpInterfaces are a laptop's, more or less: Wi-Fi, a tunnel, a
// down Ethernet port, loopback, Docker's bridges, and a LAN bridged under
// a name that begins as Docker's do.
var madeUpInterfaces = []iface{
	{name: "en0", flags: lan, addrs: []net.Addr{
		ipNet("192.168.1.50/24"),                      // LAN IPv4: kept
		ipNet("fe80::1/64"),                           // link-local: left out
		ipNet("192.168.1.50/24"),                      // twice: once
		ipNet("2001:db8::1234/64"),                    // global IPv6: kept
		ipNet("::ffff:192.168.1.51/96"),               // IPv4 written as IPv6: kept as IPv4
		ipNet("0.0.0.0/0"),                            // unspecified: left out
		&net.TCPAddr{IP: net.ParseIP("192.168.1.60")}, // no interface address
	}},
	{name: "utun6", flags: tunnel, addrs: []net.Addr{
		ipNet("100.79.132.116/32"), // tunnel: left out
		ipNet("fd7a:115c:a1e0::4f01:8474/128"),
	}},
	{name: "en5", flags: net.FlagBroadcast | net.FlagMulticast, addrs: []net.Addr{
		ipNet("10.9.9.9/8"), // down: left out
	}},
	{name: "lo0", flags: loop, addrs: []net.Addr{
		ipNet("127.0.0.1/8"), // loopback: left out
		ipNet("::1/128"),
	}},
	{name: "docker0", flags: lan, addrs: []net.Addr{
		ipNet("172.17.0.1/16"), // Docker's bridge: left out
	}},
	{name: "br-0123456789ab", flags: lan, addrs: []net.Addr{
		ipNet("172.18.0.1/16"), // a bridge of a Docker network: left out
	}},
	{name: "br-lan", flags: lan, addrs: []net.Addr{
		ipNet("10.0.0.2/24"), // OpenWrt's or NixOS's LAN bridge: kept
	}},
}

func addrPorts(ss ...string) []netip.AddrPort {
	var out []netip.AddrPort
	for _, s := range ss {
		out = append(out, netip.MustParseAddrPort(s))
	}
	return out
}

func TestAnnouncedAddresses(t *testing.T) {
	for _, tc := range []struct {
		local string
		want  []netip.AddrPort
	}{
		{"[::]:4435", addrPorts("192.168.1.50:4435", "[2001:db8::1234]:4435", "192.168.1.51:4435", "10.0.0.2:4435")},
		{"0.0.0.0:4435", addrPorts("192.168.1.50:4435", "192.168.1.51:4435", "10.0.0.2:4435")},
		{"172.17.0.1:4435", nil},
		{"192.168.1.50:4435", addrPorts("192.168.1.50:4435")},
		{"100.79.132.116:4435", nil},
		{"127.0.0.1:4435", nil},
	} {
		got := announcedAddrs(madeUpInterfaces, netip.MustParseAddrPort(tc.local))
		if !slices.Equal(got, tc.want) {
			t.Errorf("bound to %s: got %v, want %v", tc.local, got, tc.want)
		}
	}
}

func TestPublishableAddrs(t *testing.T) {
	announced := announcedAddrs(madeUpInterfaces, netip.MustParseAddrPort("[::]:1"))
	hidden := hiddenAddrs(madeUpInterfaces, announced)
	relay := netaddr.RelayAddr{}
	lanAddr := netaddr.IPAddr{Addr: netip.MustParseAddrPort("192.168.1.50:1")}
	reflexive := netaddr.IPAddr{Addr: netip.MustParseAddrPort("203.0.113.7:61234")}
	in := []netaddr.TransportAddr{
		relay,
		lanAddr,
		reflexive, // found by the net report: no interface has it
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("100.79.132.116:1")},              // tunnel
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("[fd7a:115c:a1e0::4f01:8474]:1")}, // tunnel
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("172.17.0.1:1")},                  // Docker's bridge
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("[::]:1")},
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("0.0.0.0:1")},
		netaddr.IPAddr{},
	}
	// Whatever the interfaces say: an endpoint bound to one of these
	// puts it in its own address.
	never := []netaddr.TransportAddr{
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("127.0.0.1:1")},
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("[::1]:1")},
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("[fe80::1]:1")},
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("169.254.1.1:1")},
	}
	got := publishable(append(in, never...), hidden)
	if want := []netaddr.TransportAddr{relay, lanAddr, reflexive}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := publishable(append([]netaddr.TransportAddr{relay, lanAddr}, never...), map[netip.Addr]bool{}); !slices.Equal(got, []netaddr.TransportAddr{relay, lanAddr}) {
		t.Fatalf("nothing hidden: got %v, want the relay and the LAN address", got)
	}
	// The interfaces could not be read: the relay alone.
	if got := publishable(in, nil); !slices.Equal(got, []netaddr.TransportAddr{relay}) {
		t.Fatalf("interfaces unknown: got %v, want the relay alone", got)
	}
}

// recordingAnnouncer stands for mDNS, and records what it was given.
type recordingAnnouncer struct{ calls [][]netip.AddrPort }

func (r *recordingAnnouncer) setAddrs(addrs []netip.AddrPort) {
	r.calls = append(r.calls, slices.Clone(addrs))
}

func TestAnnouncementsFollowTheAddresses(t *testing.T) {
	l, _ := testListener(t, defaultTimeouts)
	local := netip.MustParseAddrPort("[::]:4435")
	ifaces := slices.Clone(madeUpInterfaces)
	var readErr error
	read := func() ([]iface, error) { return ifaces, readErr }
	rec := &recordingAnnouncer{}
	pinned := func() []netip.AddrPort { return l.ep.Addr().IPAddrs() }
	lanAddr := netaddr.IPAddr{Addr: netip.MustParseAddrPort("192.168.1.50:4435")}
	tunnelAddr := netaddr.IPAddr{Addr: netip.MustParseAddrPort("100.79.132.116:4435")}
	relay := netaddr.RelayAddr{}

	if got := l.publishableNow([]netaddr.TransportAddr{relay, lanAddr}); !slices.Equal(got, []netaddr.TransportAddr{relay}) {
		t.Fatalf("before the interfaces were read the record holds %v, want the relay alone", got)
	}

	want := addrPorts("192.168.1.50:4435", "[2001:db8::1234]:4435", "192.168.1.51:4435", "10.0.0.2:4435")
	addrs, changed := l.refreshAddrs(read, local, rec)
	if !changed || !slices.Equal(addrs, want) || len(rec.calls) != 1 || !slices.Equal(rec.calls[0], want) {
		t.Fatalf("first reading: %v %v, mDNS given %v; want %v", addrs, changed, rec.calls, want)
	}
	for _, ap := range want {
		if !slices.Contains(pinned(), ap) {
			t.Errorf("%v is not among the endpoint's addresses %v", ap, pinned())
		}
	}
	if got := l.publishableNow([]netaddr.TransportAddr{relay, lanAddr, tunnelAddr}); !slices.Equal(got, []netaddr.TransportAddr{relay, lanAddr}) {
		t.Fatalf("the record holds %v, want the relay and the LAN address", got)
	}

	if _, changed := l.refreshAddrs(read, local, rec); changed || len(rec.calls) != 1 {
		t.Fatalf("the same addresses again: changed %v, mDNS given %v", changed, rec.calls)
	}

	ifaces[0].addrs = ifaces[0].addrs[:4] // 192.168.1.51 is gone
	want = addrPorts("192.168.1.50:4435", "[2001:db8::1234]:4435", "10.0.0.2:4435")
	if addrs, changed := l.refreshAddrs(read, local, rec); !changed || !slices.Equal(addrs, want) ||
		len(rec.calls) != 2 || !slices.Equal(rec.calls[1], want) {
		t.Fatalf("an address gone: %v %v, mDNS given %v; want %v", addrs, changed, rec.calls, want)
	}
	if slices.Contains(pinned(), netip.MustParseAddrPort("192.168.1.51:4435")) {
		t.Errorf("192.168.1.51:4435 is still among the endpoint's addresses %v", pinned())
	}

	readErr = errors.New("no interfaces today")
	if _, changed := l.refreshAddrs(read, local, rec); changed || len(rec.calls) != 2 {
		t.Fatalf("unreadable interfaces: changed %v, mDNS given %v", changed, rec.calls)
	}
	if got := l.publishableNow([]netaddr.TransportAddr{relay, lanAddr}); !slices.Equal(got, []netaddr.TransportAddr{relay}) {
		t.Fatalf("while the interfaces cannot be read the record holds %v, want the relay alone", got)
	}
	readErr = nil
	l.refreshAddrs(read, local, rec)
	if got := l.publishableNow([]netaddr.TransportAddr{relay, lanAddr}); !slices.Equal(got, []netaddr.TransportAddr{relay, lanAddr}) {
		t.Fatalf("once they can be read again the record holds %v", got)
	}
}

// TestAnnounced binds the endpoint as kosh serve does, with the relays,
// number0's DNS and mDNS, and reads in the log that the relay answered,
// that mDNS announces, and what the pkarr record holds. It needs the
// internet and a network with multicast, so it runs only when
// KOSH_NETWORK_TESTS=1.
func TestAnnounced(t *testing.T) {
	if os.Getenv("KOSH_NETWORK_TESTS") != "1" {
		t.Skip("set KOSH_NETWORK_TESTS=1 to run the test that needs the network")
	}
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l, err := Listen(ctx, Config{Key: newKey(t), Relay: true, MDNS: true, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the pkarr record", func() bool {
		return strings.Contains(logs.String(), "publishing the endpoint's pkarr record")
	})
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	t.Log(logs.String())
	for _, want := range []string{"the relay answered", "mDNS announces", "._irohv1._udp.local."} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not say %q", want)
		}
	}
}
