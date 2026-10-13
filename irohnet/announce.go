package irohnet

// Follows jaccard-store's node/serve.go (v0.7.0) and the probe's
// endpoint.go (Task 0): the wait for the relay, the direct addresses and
// the pkarr record.

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/tmc/go-iroh/dns"
	"github.com/tmc/go-iroh/iroh"
	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/netaddr"
)

// relayWait is how long Listen waits for the home relay.
const relayWait = 15 * time.Second

// addrRefresh is how often the machine's addresses are read again, so
// that the announcements follow them.
const addrRefresh = 30 * time.Second

// announce makes the endpoint findable as cfg says: it pins the announced
// addresses as direct addresses, starts mDNS, waits for the relay, keeps
// a pkarr record with number0's DNS, and follows the addresses. What it
// starts runs until the listener is closed and is counted in its
// WaitGroup. It fails only when ctx is done while it waits.
func (l *Listener) announce(ctx context.Context, cfg Config) error {
	ep, log := l.ep, l.log

	var m *mdnsAnnouncer
	if cfg.MDNS {
		var err error
		if m, err = listenMDNS(ctx, ep.ID(), log); err != nil {
			log.Warn("mDNS is unavailable; clients on the local network find the endpoint through the relay and number0's DNS only", "error", err)
			m = nil
		}
	}
	addrs, _ := l.refreshAddrs(m)
	if m != nil {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			m.run(l.ctx)
		}()
		instance, _ := mdnsNames(ep.ID())
		if len(addrs) == 0 {
			log.Warn("mDNS announces nothing yet: the endpoint has no address on the local network", "instance", instance)
		} else {
			log.Info("mDNS announces", "instance", instance, "addrs", addrs)
		}
	}

	if cfg.Relay {
		waitCtx, cancel := context.WithTimeout(ctx, relayWait)
		err := ep.Online(waitCtx)
		cancel()
		switch {
		case err == nil:
			relay := ""
			if st := ep.HomeRelayStatus().Current(); st != nil {
				relay = st.URL.String()
			}
			log.Info("the relay answered", "relay", relay)
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			log.Warn("no relay has answered; the endpoint is used through the relay once one does, and until then only by clients that reach a direct address",
				"waited", relayWait, "error", err)
		}
		if err := l.publishPkarr(cfg.Key); err != nil {
			log.Warn("no pkarr record: clients find the endpoint only on the local network", "error", err)
		}
	}

	l.wg.Add(1)
	go l.followAddrs(m)
	return nil
}

// refreshAddrs reads the machine's addresses and, when what is to be
// announced has changed, pins the new addresses as the endpoint's direct
// addresses, which makes the pkarr record anew, and hands them to m, when
// there is mDNS. It returns the addresses and whether they changed.
func (l *Listener) refreshAddrs(m *mdnsAnnouncer) ([]netip.AddrPort, bool) {
	ifaces, err := readInterfaces()
	if err != nil {
		l.log.Debug("the machine's addresses cannot be read", "error", err)
		return nil, false
	}
	addrs := announcedAddrs(ifaces, l.ep.LocalAddr())
	hidden := hiddenAddrs(ifaces, addrs)
	l.amu.Lock()
	old := l.announced
	l.announced, l.hidden = addrs, hidden
	l.amu.Unlock()
	if slices.Equal(old, addrs) {
		return addrs, false
	}
	for _, ap := range old {
		if !slices.Contains(addrs, ap) {
			l.ep.RemoveExternalAddr(ap)
		}
	}
	for _, ap := range addrs {
		l.ep.AddExternalAddr(ap)
	}
	if m != nil {
		m.setAddrs(addrs)
	}
	return addrs, true
}

// followAddrs reads the machine's addresses every addrRefresh until the
// listener is closed, and announces them anew when they changed.
func (l *Listener) followAddrs(m *mdnsAnnouncer) {
	defer l.wg.Done()
	t := time.NewTicker(addrRefresh)
	defer t.Stop()
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		}
		if addrs, changed := l.refreshAddrs(m); changed {
			l.log.Info("the announced addresses changed", "addrs", addrs)
		}
	}
}

// publishableNow is publishable with the addresses hidden at the moment:
// the address filter of the pkarr publisher.
func (l *Listener) publishableNow(addrs []netaddr.TransportAddr) []netaddr.TransportAddr {
	l.amu.Lock()
	hidden := l.hidden
	l.amu.Unlock()
	return publishable(addrs, hidden)
}

// publishPkarr keeps a pkarr record of the endpoint with number0's DNS,
// published again whenever the endpoint's addresses change: when a relay
// answers late, the home relay moves, or the machine's addresses change.
func (l *Listener) publishPkarr(key irohkey.SecretKey) error {
	pub, err := iroh.N0PkarrPublisher(key, &iroh.PkarrPublisherConfig{AddrFilter: l.publishableNow})
	if err != nil {
		return err
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer pub.Close()
		for addr := range l.ep.WatchAddr().Stream(l.ctx) {
			pub.Publish(dns.NewEndpointData(addr.Addrs()...))
			l.log.Debug("publishing the endpoint's pkarr record", "id", l.id, "addrs", l.publishableNow(addr.Addrs()))
		}
	}()
	return nil
}

// bridgePrefixes begin the names of the interfaces of containers and
// virtual machines. Their addresses cannot be reached from another machine,
// and every dead address that is advertised is one more a client waits on.
var bridgePrefixes = []string{"docker", "br-", "cni", "flannel", "veth", "virbr", "lxc"}

func isBridgeName(name string) bool {
	for _, p := range bridgePrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// iface is the name, the flags and the addresses of one network
// interface: what announcedAddrs decides on, so that it can be tested
// with interfaces the test makes up.
type iface struct {
	name  string
	flags net.Flags
	addrs []net.Addr
}

// readInterfaces lists the machine's interfaces. An interface whose
// addresses cannot be read is listed without them.
func readInterfaces() ([]iface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]iface, 0, len(ifaces))
	for _, ifc := range ifaces {
		addrs, _ := ifc.Addrs()
		out = append(out, iface{name: ifc.Name, flags: ifc.Flags, addrs: addrs})
	}
	return out, nil
}

// isLAN reports whether addresses of an interface with flags are worth
// announcing: it is up, broadcast and multicast, and neither a tunnel
// (point-to-point) nor loopback. The probe found that a tunnel's address
// in an announcement makes iroh-ssh's connections time out.
func isLAN(flags net.Flags) bool {
	return flags&net.FlagUp != 0 && flags&net.FlagBroadcast != 0 && flags&net.FlagMulticast != 0 &&
		flags&net.FlagPointToPoint == 0 && flags&net.FlagLoopback == 0
}

// ifaceIPs returns the unicast addresses of ifc, an IPv4 address written
// as IPv6 as IPv4.
func ifaceIPs(ifc iface) []netip.Addr {
	var out []netip.Addr
	for _, a := range ifc.addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip, ok := netip.AddrFromSlice(n.IP); ok {
			out = append(out, ip.Unmap())
		}
	}
	return out
}

// announcedAddrs returns the addresses that mDNS and the pkarr record
// announce for an endpoint bound to local: the addresses of the LAN
// interfaces (isLAN), less container bridges and loopback, link-local and
// unspecified addresses, each with the bound port, in the order of the
// interfaces and none twice. A socket bound to 0.0.0.0 is reached over
// IPv4 alone, and one bound to one address at that address alone, if it
// is one of them.
func announcedAddrs(ifaces []iface, local netip.AddrPort) []netip.AddrPort {
	bound := local.Addr().Unmap()
	var out []netip.AddrPort
	seen := make(map[netip.Addr]bool)
	for _, ifc := range ifaces {
		if !isLAN(ifc.flags) || isBridgeName(ifc.name) {
			continue
		}
		for _, ip := range ifaceIPs(ifc) {
			if seen[ip] || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				continue
			}
			switch {
			case !bound.IsUnspecified() && ip != bound:
				continue
			case bound.IsUnspecified() && bound.Is4() && !ip.Is4():
				continue
			}
			seen[ip] = true
			out = append(out, netip.AddrPortFrom(ip, local.Port()))
		}
	}
	return out
}

// hiddenAddrs returns the addresses of the machine's interfaces that are
// not announced: those of tunnels, bridges, loopback and the like, which
// the pkarr record leaves out although go-iroh may know them.
func hiddenAddrs(ifaces []iface, announced []netip.AddrPort) map[netip.Addr]bool {
	hidden := make(map[netip.Addr]bool)
	for _, ifc := range ifaces {
		for _, ip := range ifaceIPs(ifc) {
			hidden[ip] = true
		}
	}
	for _, ap := range announced {
		delete(hidden, ap.Addr())
	}
	return hidden
}

// publishable is what of the endpoint's addresses the pkarr record
// holds: every address but the IP addresses nobody can dial (invalid,
// unspecified) and those that are hidden. The publisher's own default
// filter would publish the relay alone.
func publishable(addrs []netaddr.TransportAddr, hidden map[netip.Addr]bool) []netaddr.TransportAddr {
	out := make([]netaddr.TransportAddr, 0, len(addrs))
	for _, a := range addrs {
		if ip, ok := a.(netaddr.IPAddr); ok &&
			(!ip.Addr.IsValid() || ip.Addr.Addr().IsUnspecified() || hidden[ip.Addr.Addr().Unmap()]) {
			continue
		}
		out = append(out, a)
	}
	return out
}
