# The QUIC key update that cut direct connections, and the issue for go-iroh

Taken from the report of the task that investigated it (Fix C of stage 1's execution, 2026-10-13). kosh's workaround is `irohnet/keyupdate.go`. The issue's text at the end has **not been filed**; its two quotations of RFC 9001 were written from memory and want checking first.

The addresses of the test machine's own networks are replaced here by documentation addresses (192.0.2.x for its LAN, 198.51.100.x for its overlay networks, 203.0.113.7 for its public address); the addresses of the kind cluster are as they were.

## A correction to the brief first

The QUIC that go-iroh v0.3.0 runs is **not** the module
`github.com/quic-go/quic-go` of `go.mod`. It is a copy of quic-go's code inside
go-iroh, `github.com/tmc/go-iroh/internal/qng`, with multipath added. The code
in question is
`~/go/pkg/mod/github.com/tmc/go-iroh@v0.3.0/internal/qng/internal/handshake/updatable_aead.go`.
The variable to reach is
`github.com/tmc/go-iroh/internal/qng/internal/handshake.FirstKeyUpdateInterval`;
setting quic-go's variable of the same name would have done nothing. The issue
belongs to go-iroh, not to quic-go: quic-go itself has one packet number space
and its check is right there.

## 1. Reproduced and counted

Setting, for every row: `framework.ffr`, client and server on the same machine,
relays off, the server found by mDNS. The server is a small program (not
committed, see "The probe" below) that serves `golang.org/x/crypto/ssh` over
`irohnet.Listen(Config{MDNS: true})`, accepts every key, and runs `true`,
`flood <MiB>` or `trickle <lines>`. The client is `iroh-ssh` built from
`82e9971` (iroh 1.3.0, noq 1.3.0), run as `scripts/test-mdns.sh` runs it:
`iroh-ssh -i key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes kosh@<id> true`,
one after the other. Failures are counted from the server's log (one line per
connection whose `Read` failed with `KEY_UPDATE_ERROR (local): received ACK for
key phase 1, but peer didn't update keys`); each such connection also ended
`iroh-ssh` with status 255, `Connection closed by UNKNOWN port 65535`.

| Run | Build | Announced | Connections | `KEY_UPDATE_ERROR` | Client status ≠ 0 |
|---|---|---|---|---|---|
| base | b448e67 | `192.0.2.155`, `198.51.100.6`, `198.51.100.196` (Wi-Fi and both ZeroTier) | 300 | **24** (8.0 %) | 25 |
| one address | b448e67, bound to `192.0.2.155:0` | `192.0.2.155` only | 300 | **16** (5.3 %) | 16 |
| first update put off, in the probe | b448e67, `FirstKeyUpdateInterval = 10^9` set by the probe | three, as base | 300 | **0** | 0 |
| fixed | the fix (irohnet sets 100,000), before the last edit of `keyupdate.go` | three | 400 | **0** | 0 |
| final | commit `f64be36` | three | 300 | **0** | 0 |

- In base, one of the 25 failed attempts never reached the server's `Accept`
  (299 connections in its log, 24 of them the error). *Corrected after the
  review (C4):* I wrote here that it "is not this bug"; that was unfounded.
  25 qlog files of base end in `key_update_error`, one more than the 24 log
  lines, so the 25th failure was most likely the same error on a connection
  that go-iroh closed before `irohnet` had handed it to `Accept` (the probe
  logs only what it accepted). The qlogs are deleted and I cannot check it
  again; base is 24 counted from the log and 25 by the qlogs and by the
  client's statuses.
- Every failing connection failed "before authenticating", as in kosh's log
  line of the first report: packet 100 comes in the middle of SSH's handshake.
- Not this bug, seen in every run, fixed or not: in about a fifth of the
  connections that succeeded (67 of 275 in base, 92 of 400 in fixed) the server's
  connection ended, after the session had ended with status 0, with `timeout: no
  recent network activity` instead of the client's close. The client has its
  result by then; kosh holds such a connection until the idle timeout. I did not
  look into it.

**Does it depend on …**

- *the number of direct addresses kosh announces:* a little. With one announced
  address it is 16 of 300 against 24 of 300. The client still opens many paths:
  the qlog shows path IDs 0 to 8 (the client has several addresses of its own).
- *mDNS against relay discovery:* not measured. The cause (below) does not
  depend on how the client learned the addresses, only on the client using more
  than one path; over the relays with iroh 0.94 (released `iroh-ssh` 0.2.12,
  no multipath) the first report had 40 of 40.
- *the client being on the same machine:* not measured; no second machine with
  an `iroh-ssh` on iroh 1.x was at hand. Nothing in the cause needs it.

## 2. The cause: (b), a false alarm of go-iroh's key update check under multipath

All paths in `~/go/pkg/mod/github.com/tmc/go-iroh@v0.3.0/internal/qng/`.

- `internal/handshake/updatable_aead.go:31`: `var FirstKeyUpdateInterval uint64 = 100`;
  `:329-337` (`shouldInitiateKeyUpdate`): in key phase 0 the keys are updated
  once 100 packets were sent or received, "in order to exercise the key update
  mechanism".
- `:288-298` (`Seal`): takes the path and the packet number, and records
  `a.firstSentWithCurrentKey = pn`: **one number for the connection, of
  whichever path sent first after the update.** (The same file keeps
  `highestRcvdPN` per path, `:50-57`, with a comment that says why: "each path
  has its own packet-number space". The key update bookkeeping was not given
  the same treatment.)
- `:301-310` (`SetLargestAcked(pn)`): `pn >= a.firstSentWithCurrentKey &&
  a.numRcvdWithCurrentKey == 0` is the error. It has no path.
- `connection.go:2551-2557` (`handlePathAckFrame`) → `:2950-2974`
  (`handleAckFrameForPath`), whose last line is
  `c.cryptoStreamHandler.SetLargest1RTTAcked(frame.LargestAcked())`: the largest
  acknowledged number **of any path** goes into that comparison
  (`internal/handshake/crypto_setup.go:184`).

So: when the first packet of the new key phase goes out on a path with low
numbers, and an acknowledgement for older packets of a path with higher numbers
arrives before the peer's first packet in the new phase, the comparison is
between numbers of two different spaces and reads as a violation.

The qlog of a failing connection (server side, `QLOGDIR`; the last lines, cut
to what matters) shows exactly that:

```
2.851243 packet_sent     pn 84 dcid 756decfe… key_phase 0  ack [26,33]                       <- path 0, last packet of phase 0
2.87365  key_updated     trigger local_update, key_phase 1
2.87485  packet_sent     pn 3  dcid 7484ffba… key_phase 1  path_ack path_id 1 [0,2]          <- first packet of phase 1: number 3, path 1
2.886409 packet_sent     pn 3  dcid 6e47bf1c… key_phase 1  path_ack path_id 2 [0,2]
   … path_ack for paths 3, 4, 7, numbers 3, 2, 1 …
2.943855 packet_received pn 34 key_phase 0  path_response, path_challenge
2.947866 packet_sent     pn 85 dcid 756decfe… key_phase 1  path_response                     <- path 0, first packet of phase 1 there
2.972646 packet_received pn 35 key_phase 0  path_ack path_id 0 acked [81,84], paths_blocked 8
2.986057 connection_closed local, key_update_error, "received ACK for key phase 1, but peer didn't update keys"
```

The peer acknowledged packets 81 to 84 of path 0. All four were sent in key
phase 0 (the first of phase 1 on path 0 is 85). The peer acknowledged nothing
of the new phase and broke no rule of RFC 9001 §6; go-iroh compared 84 with
path 1's number 3. Explanation (a) is ruled out for this connection. I read
this one qlog; the other 24 of base that end in the error (25 files in all, see
1 for why one more than the log's 24) I did not read line by line: they end in
the same error, raised at the one place in the code that raises it.

Why at the start, and why an iroh 1.x client: such a client opens and probes
its paths (PING, PATH_CHALLENGE on path IDs 1 to 8) while the connection
begins, so the server's packet right after its 100th is often an
acknowledgement on a new path, with a number below 10.

The same reproduced without any network, as a unit test inside go-iroh's
package (run in a copy of the module, fails on v0.3.0); it is in the issue's
text below.

## 3. Does it also end sessions later, at the regular key updates?

The regular update (every 100,000 packets, `internal/protocol/params.go:151`)
goes through the same check, so the bug is there in principle. It was not seen:

| Run | Build and settings | Sessions | Key updates by the server | `KEY_UPDATE_ERROR` |
|---|---|---|---|---|
| long | the fix, nothing else changed (first and regular update at 100,000) | 5 × `flood 2000` (2000 MiB each, all bytes arrived, status 0) | about 75 (a qlog of a 400 MiB session: 307,807 packets sent, 2 local updates and 1 by the peer) | 0 |
| regular, dense | first update off, regular interval 2,000 (set by the probe) | 10 × `flood 300` | about 1,150 (qlog of a 50 MiB session: 38,540 packets, 19 updates) | 0 |
| regular, interactive | first update off, regular interval 100 | 8 at once × `trickle 6000` (a line every 10 ms for 66 s) | about 500 (qlog: 62 per session) | 0 |

Why it does not strike there: once a session runs, nearly every packet the
server sends is on the path that carries the data (in the trickle qlog the
other seven paths got 5 acknowledgements each in 66 s, against 3,055 on path
0), so the first packet after an update is almost always on that path, and the
comparison is then within one number space. At the start the mix is the
opposite.

**What this does not prove:** that a regular update can never hit it. A session
whose 100,000th packet is followed first by a packet on a quiet path, with an
acknowledgement of the busy path in flight, would be closed. The numbers above
are 0 failures in about 1,700 updates, which bounds the rate at about one
update in 570 with 95 % confidence, no better (*corrected after the review,
C2:* I had written "less than one in about 1,700"); a kosh session reaches
100,000 packets after hours of use or minutes of a fast log.


## 6. The issue for go-iroh (not filed)

**Title:** qng: false `KEY_UPDATE_ERROR` with multipath: the key update check compares packet numbers of different paths

**Text:**

go-iroh v0.3.0, server side, with an iroh 1.3.0 (noq 1.3.0) client on a direct
path. About one connection in 13 is closed by go-iroh shortly after the
handshake with

```
KEY_UPDATE_ERROR (local): received ACK for key phase 1, but peer didn't update keys
```

The peer did not violate RFC 9001. `updatableAEAD` keeps the key update
bookkeeping in one packet number space, while with multipath every path has
its own:

- `internal/qng/internal/handshake/updatable_aead.go`, `Seal`: `a.firstSentWithCurrentKey = pn`
  is set from the first packet sealed after a key update, on whichever path
  (`pid` is passed in and used for the nonce only).
- `SetLargestAcked(pn)`: `pn >= a.firstSentWithCurrentKey && a.numRcvdWithCurrentKey == 0`
  is the error. It takes no path.
- `internal/qng/connection.go`, `handleAckFrameForPath` (and `handleAckFrame`):
  `c.cryptoStreamHandler.SetLargest1RTTAcked(frame.LargestAcked())` is called
  for a PATH_ACK of any path.

So if the first packet of the new key phase is sent on path 1 with packet
number 3, and a PATH_ACK for path 0 acknowledging packets 81 to 84 (all sent
before the update) arrives before the peer's first packet in the new phase,
`84 >= 3` closes the connection. Because `FirstKeyUpdateInterval` is 100, the
first update falls into the time when an iroh client is opening and probing its
paths, and the first packet after the update is often a PATH_ACK on a new path.

qlog of such a connection (server):

```
2.851243 packet_sent     pn 84 dcid 756decfe… key_phase 0  ack [26,33]
2.87365  key_updated     trigger local_update, key_phase 1
2.87485  packet_sent     pn 3  dcid 7484ffba… key_phase 1  path_ack path_id 1 [0,2]
2.947866 packet_sent     pn 85 dcid 756decfe… key_phase 1  path_response
2.972646 packet_received pn 35 key_phase 0  path_ack path_id 0 acked [81,84], paths_blocked 8
2.986057 connection_closed local, key_update_error, "received ACK for key phase 1, but peer didn't update keys"
```

RFC 9001 §6.2: "An endpoint that receives an acknowledgment that is carried in
a packet protected with old keys where any acknowledged packet was protected
with newer keys MAY treat that as a connection error of type
KEY_UPDATE_ERROR." No acknowledged packet here was protected with the newer
keys. (And §6.1 on the peer's duty: "Sending keys MUST be updated before
sending an acknowledgment for the packet that was received with updated keys":
the peer had received none.) Both quotations are from memory of the RFC's
text: compare them with the RFC before filing.
draft-ietf-quic-multipath gives each path its own packet number space, so
"acknowledged packet" has to be looked up per path.

To reproduce without a network, as
`internal/qng/internal/handshake/key_update_multipath_test.go` (fails on
v0.3.0 with the error above):

```go
package handshake

import (
	"testing"

	tls "github.com/tmc/go-iroh/internal/itls/tls"
	"github.com/tmc/go-iroh/internal/qng/internal/monotime"
	"github.com/tmc/go-iroh/internal/qng/internal/protocol"
	"github.com/tmc/go-iroh/internal/qng/internal/utils"
)

// An ACK for packets that were sent on path 0 before a key update must not be
// taken for an ACK of the new key phase because the first packet of the new
// phase went out on path 1, whose packet numbers are lower.
func TestKeyUpdateWithPacketNumbersOfTwoPaths(t *testing.T) {
	suite := getCipherSuite(tls.TLS_AES_128_GCM_SHA256)
	secretA, secretB := make([]byte, 32), make([]byte, 32)
	secretB[0] = 1
	a := newUpdatableAEAD(utils.NewRTTStats(), nil, utils.DefaultLogger, protocol.Version1)
	b := newUpdatableAEAD(utils.NewRTTStats(), nil, utils.DefaultLogger, protocol.Version1)
	a.SetWriteKey(suite, secretA)
	a.SetReadKey(suite, secretB)
	b.SetWriteKey(suite, secretB)
	b.SetReadKey(suite, secretA)
	a.SetHandshakeConfirmed()

	ad := []byte("header")
	now := monotime.Now()
	// Path 0 carries the connection: a sends packets 0 to 84 there, and
	// receives enough from b to reach FirstKeyUpdateInterval.
	for pn := protocol.PacketNumber(0); pn <= 84; pn++ {
		if kp := a.KeyPhase(); kp != protocol.KeyPhaseZero {
			t.Fatalf("key update before packet %d was sent", pn)
		}
		a.Seal(nil, []byte("data"), protocol.PathIDZero, pn, ad)
	}
	for pn := protocol.PacketNumber(0); pn < protocol.PacketNumber(FirstKeyUpdateInterval); pn++ {
		sealed := b.Seal(nil, []byte("data"), protocol.PathIDZero, pn, ad)
		if _, err := a.Open(nil, sealed, now, protocol.PathIDZero, pn, protocol.KeyPhaseZero, ad); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.SetLargestAcked(80); err != nil {
		t.Fatal(err)
	}

	// a updates its keys; the first packet of key phase 1 happens to be
	// packet 3 of path 1 (a PATH_ACK for the peer's probes there).
	if kp := a.KeyPhase(); kp != protocol.KeyPhaseOne {
		t.Fatal("no key update after FirstKeyUpdateInterval packets")
	}
	a.Seal(nil, []byte("path ack"), protocol.PathID(1), 3, ad)

	// b's PATH_ACK{path 0, 81..84}, sent in key phase 0 before b saw the
	// update, arrives. Packets 81 to 84 of path 0 were sent in key phase 0.
	if err := a.SetLargestAcked(84); err != nil {
		t.Fatalf("an ACK for packets of path 0 that were sent before the key update: %v", err)
	}
}
```

To reproduce with a peer: any go-iroh endpoint that accepts a connection, and
an iroh 1.3.0 client that has more than one local address, with `QLOGDIR` set;
exchange a few hundred small packets right after connecting. Here 24 of 300
connections failed.

Suggested fix: keep `firstSentWithCurrentKey` (and the check in
`SetLargestAcked`, and `largestAcked` for `updateAllowed`) per `PathID`, as
`highestRcvdPN` already is; an acknowledgement for a path on which nothing was
sent in the current phase acknowledges nothing of that phase. By reading, not
by measurement: `open` chooses the previous keys with `pn <
a.firstRcvdWithCurrentKey`, which is one number for all paths too, so after a
peer's update a packet of another path with a lower number would be tried with
the wrong keys and dropped.

The same bookkeeping serves a key update that the peer starts, and noq
(iroh 1.x) starts its first one after a random 10 to 999 packets
(`noq-proto` 1.3.0, `src/connection/packet_crypto.rs:130-136`), so raising
`FirstKeyUpdateInterval` does not remove the early update, only the one go-iroh
starts. After a peer's update (`updatable_aead.go:226-235`, `:249-267`)
`numRcvdWithCurrentKey` is 0 and the packet that brought the update is not
counted; once go-iroh has sent in the new phase on a path with low numbers, an
old-phase packet that arrives late (opened with the previous keys, chosen by
`pn < a.firstRcvdWithCurrentKey`, again one number for all paths) with a
PATH_ACK for higher numbers of another path raises the same error. Not
observed here; it follows from the code.

Independently, `FirstKeyUpdateInterval` (100) and the regular interval are
reachable only as package variables of an internal package; an option of the
endpoint would let a user of the library work around this kind of thing.

Workaround in use: raise `handshake.FirstKeyUpdateInterval` to the regular
interval through `go:linkname`; 0 of 700 connections failed with it. It covers
only the update go-iroh starts.


## From Fix C's review

- **C1 (Important).** Checked and true: `noq-proto` 1.3.0,
  `src/connection/packet_crypto.rs:130-136`, `key_phase_size:
  rng.random_range(10..1000)`; go-iroh's `updatable_aead.go:226-235` and
  `:249-267` as the review reads them. Corrected:
  - `irohnet/keyupdate.go` (`b5b09cd`, comment only): the title and "What is
    set" now speak of the update this side starts; a new paragraph "What is not
    covered" names the peer's early update (with noq's file), why the false
    error is rarer after it (the packet that brings the update and those after
    it are in the new phase, and the first that arrives ends the window; only
    an old-phase packet arriving late across paths is misread), and the
    regular updates.
  - `fix-c-report.md`: §4's sentence replaced and the correction written out;
    §6 (the issue's text) has the peer-started case with those line numbers,
    and says the workaround covers only the update go-iroh starts; the note on
    what could not be found out names it; a line at the top says the report
    was corrected.
  - **Measured:** 100 connections, git `iroh-ssh` on the box, the probe of Fix
    C built on this tree (first update at 100,000), with qlog: 100 of 100 ok, 0
    `KEY_UPDATE_ERROR`; **4 of the 100 connections had a key update started by
    the peer** at their start (after 46, 48, 62 and 65 packets received by the
    server), none started by kosh's side. A connection of this kind brings the
    server 66 to 104 packets (median 80), so a peer whose threshold is drawn
    from 10 to 999 gets there in a few of a hundred. Fix C's 700 clean
    connections will have held about 30 such updates; that is an extrapolation,
    their qlogs were not kept. So: peer-started early updates do happen in
    fixed connections, 4 were seen and survived, and 4 says nothing about a
    rate of failure.
- **C2.** `fix-c-report.md` §3 now says 0 failures in about 1,700 updates
  bound the rate at about one in 570 with 95 % confidence, no better.
- **C3.** `fix-c-report.md` §4 now names go-iroh's own QUIC connections to the
  relays (`internal/netreport/qad.go`) as affected too, harmlessly.
- **C4.** Both numbers were right and one sentence was wrong. 24 is the count
  of the server's log lines; 25 is the count of qlog files of that run that
  end in `key_update_error`, and also of client failures. The 25th was a
  connection that failed before `irohnet` handed it to `Accept`, so the probe
  logged nothing; I had written of that attempt "it is not this bug", without
  ground. Most likely it was this bug; the qlogs are deleted, so it stays
  "most likely". Corrected in §1 and §2 of that report, where I also say that
  I read one of the 25 qlogs line by line, not all.

