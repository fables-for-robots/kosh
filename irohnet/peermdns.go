package irohnet

// The mDNS announcer is the probe's peermdns.go (Task 0), made part of
// the package: an announcer that iroh-ssh, through the crate
// iroh-mdns-peer-lookup 0.1.2, can resolve.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	irohkey "github.com/tmc/go-iroh/key"
	"golang.org/x/net/dns/dnsmessage"
)

const (
	// mdnsService is the service name iroh-ssh asks for.
	mdnsService = "_irohv1._udp.local."
	// mdnsTTL is the TTL of the records announced, in seconds.
	mdnsTTL = 120
	// classFlush is class IN with the mDNS cache-flush bit: the record
	// set replaces what a listener has cached for the name.
	classFlush = dnsmessage.ClassINET | 1<<15
)

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
