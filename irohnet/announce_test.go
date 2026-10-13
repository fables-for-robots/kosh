package irohnet

import (
	"context"
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

func TestAdvertisedAddrPorts(t *testing.T) {
	in := []ifaceAddrs{
		{name: "en0", up: true, addrs: []net.Addr{
			ipNet("192.168.1.50/24"),   // LAN IPv4: keep
			ipNet("fe80::1/64"),        // link-local: drop
			ipNet("192.168.1.50/24"),   // duplicate: drop
			ipNet("2001:db8::1234/64"), // global IPv6: keep
		}},
		{name: "lo0", up: true, addrs: []net.Addr{
			ipNet("127.0.0.1/8"), // loopback: drop
			ipNet("::1/128"),     // loopback: drop
		}},
		{name: "en5", up: false, addrs: []net.Addr{
			ipNet("10.9.9.9/8"), // interface down: drop
		}},
		{name: "cni0", up: true, addrs: []net.Addr{
			ipNet("10.42.1.1/24"), // container bridge: drop
		}},
		{name: "en1", up: true, addrs: []net.Addr{
			ipNet("::ffff:192.168.1.9/96"),                // IPv4 written as IPv6: keep as IPv4
			ipNet("0.0.0.0/0"),                            // unspecified: drop
			&net.TCPAddr{IP: net.ParseIP("192.168.1.60")}, // not an interface address: drop
		}},
	}
	got := advertisedAddrPorts(in, 4242)
	want := []netip.AddrPort{
		netip.MustParseAddrPort("192.168.1.50:4242"),
		netip.MustParseAddrPort("[2001:db8::1234]:4242"),
		netip.MustParseAddrPort("192.168.1.9:4242"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDirectAddrsFollowTheBoundSocket(t *testing.T) {
	ifaces := []ifaceAddrs{
		{name: "eth0", up: true, addrs: []net.Addr{ipNet("203.0.113.7/24"), ipNet("2001:db8::7/64")}},
		{name: "lo", up: true, addrs: []net.Addr{ipNet("127.0.0.1/8")}},
	}
	for _, tc := range []struct {
		local string
		want  []string
	}{
		{"[::]:4435", []string{"203.0.113.7:4435", "[2001:db8::7]:4435"}},
		{"0.0.0.0:4435", []string{"203.0.113.7:4435"}},
		{"203.0.113.7:4435", []string{"203.0.113.7:4435"}},
		{"127.0.0.1:4435", nil},
	} {
		var want []netip.AddrPort
		for _, s := range tc.want {
			want = append(want, netip.MustParseAddrPort(s))
		}
		if got := directAddrs(ifaces, netip.MustParseAddrPort(tc.local)); !slices.Equal(got, want) {
			t.Errorf("bound to %s: got %v, want %v", tc.local, got, want)
		}
	}
}

func TestPublishableAddrsDropsWildcard(t *testing.T) {
	relay := netaddr.RelayAddr{}
	keep := netaddr.IPAddr{Addr: netip.MustParseAddrPort("192.168.1.9:1")}
	in := []netaddr.TransportAddr{
		relay,
		keep,
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("[::]:1")},
		netaddr.IPAddr{Addr: netip.MustParseAddrPort("0.0.0.0:1")},
		netaddr.IPAddr{},
	}
	got := publishableAddrs(in)
	if len(got) != 2 || got[0] != relay || got[1] != keep {
		t.Fatalf("got %v", got)
	}
}

// TestAnnounced binds the endpoint as kosh serve does, with the relays,
// number0's DNS and mDNS, and reads in the log that the relay answered
// and that mDNS announces. It needs the internet and a network with
// multicast, so it runs only when KOSH_NETWORK_TESTS=1.
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
	t.Log(logs.String())
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, want := range []string{"the relay answered", "mDNS announces"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("the log does not say %q", want)
		}
	}
}
