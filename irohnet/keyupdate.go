package irohnet

import (
	"fmt"
	_ "unsafe" // for go:linkname
)

// kosh no longer starts a QUIC key update early in a connection.
//
// What it works around. go-iroh v0.3.0's QUIC (internal/qng, quic-go's
// code with multipath added) updates a connection's keys for the first
// time after 100 packets, "in order to exercise the key update
// mechanism", and after that every 100,000. Its check that the peer does
// not acknowledge a packet of the new key phase before it has itself
// updated (updatableAEAD.SetLargestAcked, RFC 9001 section 6.2) keeps
// one "first packet number sent with the current key" for the whole
// connection, while with multipath every path counts its packets from 0:
// when the first packet after the update goes out on a path with low
// numbers (number 3 on path 1, say) and an acknowledgement for another
// path's older packets (81 to 84 on path 0) is still on its way, 84 >= 3
// reads as "the peer acknowledged the new phase without updating" and
// go-iroh closes the connection with KEY_UPDATE_ERROR. The peer did
// nothing wrong. An iroh 1.x client opens its paths while a connection
// begins, which is when packet 100 comes, in the middle of SSH's
// handshake.
//
// How it was measured. iroh-ssh at 82e9971 (iroh 1.3.0) against an SSH
// server over this package on one Linux machine, direct, found by mDNS:
// the measurements are in the reports of the fix
// (.superpowers/sdd/2026-10-13-kosh-stage-1/fix-c-report.md and
// fix-i-report.md).
//
// What is set. The first update that this side starts waits for as many
// packets as every later one: no key update is disabled. The limits that
// need one are those of RFC 9001 section 6.6: 2^23 packets under one key
// for AES-GCM (which is 83 times firstKeyUpdateInterval), more for
// ChaCha20-Poly1305.
//
// What is not covered. The peer starts key updates too, and an iroh 1.x
// client starts its first one early: after a random 10 to 999 packets it
// has sent (noq-proto 1.3.0, connection/packet_crypto.rs,
// key_phase_size). go-iroh handles an update the peer started with the
// same bookkeeping, one number for all paths, so the same false
// KEY_UPDATE_ERROR is possible after it. It needs more to go wrong: the
// packet that brings the peer's update is itself in the new phase, and
// so is what the peer sends after it, and the first of those that
// arrives ends the window; only a packet of the old phase that arrives
// late, after this side has sent in the new phase on a path with low
// numbers, and that acknowledges higher numbers of another path, is
// misread. And the regular updates, this side's every 100,000 packets,
// go through the same check. Neither was seen to fail; neither is
// excluded.
//
// When to remove. When go-iroh keeps the check per path, or lets the
// interval be configured: this file and its test go then.
//
// How it is reached. The variable is in an internal package of go-iroh,
// so by go:linkname. A linker does not complain when a variable it is
// told to share does not exist: it makes a new one, and setting that
// would do nothing, silently. So, first, the function next to the
// variable in the same file of go-iroh is linked too, and a function
// that does not exist fails the build ("relocation target … not
// defined"); and second, the variable must hold go-iroh's 100 before it
// is set, or the program does not start. (Assembly, which could make the
// build fail for the variable itself, cannot name a package whose path
// has a hyphen.)

// firstKeyUpdateInterval is how many packets a connection sends or
// receives before this side updates its keys for the first time: as
// many as between its later updates (go-iroh's
// protocol.KeyUpdateInterval). The peer may update them sooner.
const firstKeyUpdateInterval = 100_000

// qngFirstKeyUpdateIntervalDefault is what go-iroh v0.3.0 sets
// qngFirstKeyUpdateInterval to.
const qngFirstKeyUpdateIntervalDefault = 100

// qngFirstKeyUpdateInterval is go-iroh's handshake.FirstKeyUpdateInterval.
//
//go:linkname qngFirstKeyUpdateInterval github.com/tmc/go-iroh/internal/qng/internal/handshake.FirstKeyUpdateInterval
var qngFirstKeyUpdateInterval uint64

// qngSetKeyUpdateInterval is go-iroh's handshake.SetKeyUpdateInterval,
// declared beside FirstKeyUpdateInterval. It is here so that the build
// fails when that package is no longer what this file was written for.
//
//go:linkname qngSetKeyUpdateInterval github.com/tmc/go-iroh/internal/qng/internal/handshake.SetKeyUpdateInterval
func qngSetKeyUpdateInterval(v uint64) (reset func())

// keepLinked holds qngSetKeyUpdateInterval and is never called. It is
// set in init, which is what keeps the reference in the program: the
// linker drops a variable nothing reads together with what it was
// declared to hold, and the build would not fail.
var keepLinked func(uint64) func()

func init() {
	keepLinked = qngSetKeyUpdateInterval
	if err := deferFirstKeyUpdate(); err != nil {
		panic(err)
	}
}

// deferFirstKeyUpdate sets go-iroh's interval before the first key
// update to firstKeyUpdateInterval. It fails, and sets nothing, when the
// variable does not hold go-iroh's default: then it is not go-iroh's
// variable, or go-iroh changed and this file has to be looked at again.
func deferFirstKeyUpdate() error {
	if qngFirstKeyUpdateInterval != qngFirstKeyUpdateIntervalDefault {
		return fmt.Errorf("irohnet: go-iroh's FirstKeyUpdateInterval is %d, not %d: "+
			"go-iroh changed, and irohnet/keyupdate.go no longer puts off the first key update",
			qngFirstKeyUpdateInterval, qngFirstKeyUpdateIntervalDefault)
	}
	qngFirstKeyUpdateInterval = firstKeyUpdateInterval
	return nil
}
