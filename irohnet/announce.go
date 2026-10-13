package irohnet

// Follows jaccard-store's node/serve.go (v0.7.0): the wait for the relay,
// the direct addresses, the pkarr record and mDNS.

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/tmc/go-iroh/dns"
	"github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/iroh/mdns"
	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/netaddr"
)

// relayWait is how long Listen waits for the home relay.
const relayWait = 15 * time.Second

// mdnsGrace is how long Listen gives mDNS to fail before it says that
// mDNS announces. Binding the mDNS socket fails at once or not at all.
const mdnsGrace = 250 * time.Millisecond

// announce makes the endpoint findable as cfg says: it pins the addresses
// of the machine's interfaces as direct addresses, starts mDNS, waits for
// the relay and keeps a pkarr record with number0's DNS. What it starts
// runs until the listener is closed and is counted in its WaitGroup. It
// fails only when ctx is done while it waits.
func (l *Listener) announce(ctx context.Context, cfg Config) error {
	ep, log := l.ep, l.log

	var direct []netip.AddrPort
	if ifaces, err := localIfaceAddrs(); err != nil {
		log.Warn("the machine's addresses cannot be read; only the relay can reach the endpoint", "error", err)
	} else {
		direct = directAddrs(ifaces, ep.LocalAddr())
	}
	for _, ap := range direct {
		ep.AddExternalAddr(ap)
	}

	var mdnsDone <-chan error
	if cfg.MDNS {
		if len(direct) == 0 {
			log.Warn("mDNS does not announce: the endpoint has no address on the local network")
		} else {
			mdnsDone = startMDNS(l.ctx, &l.wg, ep.ID(), direct, log)
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

	if mdnsDone != nil {
		t := time.NewTimer(mdnsGrace)
		defer t.Stop()
		select {
		case <-mdnsDone:
			// mDNS ended at once; startMDNS has said why.
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			log.Info("mDNS announces", "addrs", direct)
		}
	}
	return nil
}

// publishPkarr keeps a pkarr record of the endpoint with number0's DNS,
// published again whenever the endpoint's addresses change: when a relay
// answers late, or the home relay moves.
func (l *Listener) publishPkarr(key irohkey.SecretKey) error {
	pub, err := iroh.N0PkarrPublisher(key, &iroh.PkarrPublisherConfig{AddrFilter: publishableAddrs})
	if err != nil {
		return err
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		defer pub.Close()
		for addr := range l.ep.WatchAddr().Stream(l.ctx) {
			addrs := publishableAddrs(addr.Addrs())
			pub.Publish(dns.NewEndpointData(addrs...))
			l.log.Debug("publishing the endpoint's pkarr record", "id", l.id, "addrs", addrs)
		}
	}()
	return nil
}

// startMDNS announces the endpoint id at addrs on the local network with
// go-iroh's mDNS, under the service name iroh-ssh asks for, until ctx is
// done. The goroutine it starts is counted in wg. The returned channel
// receives what the mDNS responder ended with; an error is also logged
// as a warning, and the endpoint goes on without mDNS.
func startMDNS(ctx context.Context, wg *sync.WaitGroup, id irohkey.EndpointID, addrs []netip.AddrPort, log *slog.Logger) <-chan error {
	tas := make([]netaddr.TransportAddr, len(addrs))
	for i, ap := range addrs {
		tas[i] = netaddr.IPAddr{Addr: ap}
	}
	d := mdns.New(id, mdns.WithLogger(log))
	d.Publish(dns.NewEndpointData(tas...))

	done := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := d.Start(ctx)
		if err != nil && ctx.Err() == nil {
			log.Warn("mDNS is unavailable; clients on the local network find the endpoint through the relay and number0's DNS only", "error", err)
		}
		done <- err
	}()
	return done
}

// ifaceAddrs is the name, the state and the addresses of one network
// interface: what advertisedAddrPorts decides on, so that it can be tested
// without real interfaces.
type ifaceAddrs struct {
	name  string
	up    bool
	addrs []net.Addr
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

// advertisedAddrPorts pairs the unicast addresses of the machine's
// interfaces with the port the endpoint is bound to: the direct addresses
// worth advertising. Interfaces that are down, container bridges, and
// loopback, link-local and unspecified addresses are left out. An IPv4
// address written as IPv6 is advertised as IPv4. No address is listed
// twice, and the order of the interfaces is kept.
func advertisedAddrPorts(ifaces []ifaceAddrs, port uint16) []netip.AddrPort {
	var out []netip.AddrPort
	seen := make(map[netip.Addr]bool)
	for _, ifc := range ifaces {
		if !ifc.up || isBridgeName(ifc.name) {
			continue
		}
		for _, a := range ifc.addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			if seen[ip] || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				continue
			}
			seen[ip] = true
			out = append(out, netip.AddrPortFrom(ip, port))
		}
	}
	return out
}

// directAddrs returns the addresses at which an endpoint bound to local can
// be dialed directly. A socket bound to one address is reached at that one
// and no other. A socket bound to every address is reached at the addresses
// of the interfaces, of both families or, when it was bound as 0.0.0.0, of
// IPv4 alone.
func directAddrs(ifaces []ifaceAddrs, local netip.AddrPort) []netip.AddrPort {
	addr := local.Addr().Unmap()
	if !addr.IsUnspecified() {
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() {
			return nil
		}
		return []netip.AddrPort{netip.AddrPortFrom(addr, local.Port())}
	}
	all := advertisedAddrPorts(ifaces, local.Port())
	if !addr.Is4() {
		return all
	}
	var v4 []netip.AddrPort
	for _, ap := range all {
		if ap.Addr().Is4() {
			v4 = append(v4, ap)
		}
	}
	return v4
}

// localIfaceAddrs lists the machine's interfaces for advertisedAddrPorts.
// An interface whose addresses cannot be read is left out.
func localIfaceAddrs() ([]ifaceAddrs, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]ifaceAddrs, 0, len(ifaces))
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		out = append(out, ifaceAddrs{name: ifc.Name, up: ifc.Flags&net.FlagUp != 0, addrs: addrs})
	}
	return out, nil
}

// publishableAddrs is the address filter of the pkarr publisher: every
// address but the IP addresses nobody can dial, the invalid and the
// unspecified. The publisher's own default would publish the relay alone
// and drop the direct addresses that are advertised on purpose.
func publishableAddrs(addrs []netaddr.TransportAddr) []netaddr.TransportAddr {
	out := make([]netaddr.TransportAddr, 0, len(addrs))
	for _, a := range addrs {
		if ip, ok := a.(netaddr.IPAddr); ok && (!ip.Addr.IsValid() || ip.Addr.Addr().IsUnspecified()) {
			continue
		}
		out = append(out, a)
	}
	return out
}
