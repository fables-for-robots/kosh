# Task 4: irohnet

Where kosh meets iroh: one endpoint that accepts connections for `iroh-ssh`'s protocol and hands each out as a `net.Conn` that SSH runs in. It is a `net.Listener`, so the SSH server serves it exactly as it serves the TCP port. This package knows nothing of SSH.

**Read first:** the spec, §2, §4.2 and §20 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the contract: `irohnet/irohnet.go`; **what the probe found**: `docs/superpowers/plans/2026-10-13-kosh-stage-1/task-00-probe.md`, which has the code that was seen to work against the real `iroh-ssh` and every surprise on the way. Build on that code; do not rediscover it.

**Also read**, as models (do not import them): how the sibling projects bind and announce a go-iroh endpoint, `~/go/pkg/mod/github.com/amber-store/jaccard-store@v0.7.0/node/serve.go`, and how they dial one without a relay, `node/dial.go` beside it; go-iroh itself at `~/go/pkg/mod/github.com/tmc/go-iroh@v0.3.0/iroh/` (`endpoint.go`, `conn.go`, `mdns/`).

**Files:**
- Modify: `irohnet/irohnet.go` (the bodies, and the fields of `Listener`; signatures, constants and documentation stay)
- Create: further files of the package as the code wants them (`announce.go` for relay, pkarr and mDNS; `conn.go` for the `net.Conn` a connection is handed out as; `dial.go` for `DialLocal`)
- Create: `irohnet/irohnet_test.go`, and tests beside the other files

**Interfaces:**
- Consumes: `github.com/tmc/go-iroh/iroh`, `github.com/tmc/go-iroh/iroh/mdns`, `github.com/tmc/go-iroh/key`.
- Produces, exactly as `irohnet/irohnet.go` declares them: `ALPN`, `StreamTimeout`, `Config`, `Listener` (`ID`, `Accept`, `Close`, `Addr`, `LocalAddrs`), `Addr`, `Listen`, `DialLocal`.

## Behaviour

**Listen**

1. `Listen` binds an endpoint with `cfg.Key` and the ALPN `/iroh/ssh`, on `cfg.Bind`, or on every address and a port the system picks when that is the zero value.
2. `cfg.Local`: bound to `127.0.0.1` unless `Bind` says otherwise, no relay, nothing announced, nothing looked up. `Relay` and `MDNS` are not looked at. This is what every test of this package and of `sshd` uses: no test touches the network.
3. Without `Local`, neither `Relay` nor `MDNS` is an error: nobody could find the endpoint.
4. `cfg.Relay`: the default relays, and a pkarr record with number0's DNS that follows the endpoint's addresses, as the probe's code does. `Listen` waits for the relay, 15 seconds at most; when it has not answered by then, a warning is logged and `Listen` returns the listener all the same. A relay that comes later is used.
5. `cfg.MDNS`: the endpoint is announced on the local network by an announcer of kosh's own, `irohnet/peermdns.go`, which is the probe's `peermdns.go` made part of the package: go-iroh's `iroh/mdns` cannot be found by `iroh-ssh` (its names are in another base32, and it sends no TXT record, which `iroh-ssh` waits for), so it is not used. mDNS that cannot be had (no multicast interface, the port taken) is a warning in the log, and the listener works without it.
5a. **No address of a tunnel is announced**, by mDNS or in the pkarr record: only the addresses of interfaces that are up, broadcast, multicast and not point-to-point, less loopback, link-local and unspecified addresses, each with the endpoint's port. The probe found that a tunnel's address in the announcement makes connections time out. One function returns that list; both announcers use it, and it is tested with interfaces the test makes up (give it the interfaces as an argument, with `net.Interfaces` as the caller's default).
5b. The list is read again every 30 seconds, and when it has changed the mDNS announcement and the pkarr record are made anew. The probe's announcer was given its addresses once; kosh's follows them.
5c. The announcer answers a browse of the whole service at most once a second and a burst of questions about its own instance once per 50 ms, as the probe's does, and sends a goodbye (TTL 0) when it stops. What builds the packet is a function of its own, tested by parsing the packet it returns (`golang.org/x/net/dns/dnsmessage`): the PTR, the SRV with the port and the z-base-32 host, a TXT that is present even with one address (one empty string) and carries every further address as `a<N>=` followed by the address bytes and the port, and the A or AAAA of the first address.
6. The context bounds `Listen` alone. What `Listen` starts lives until `Close`.
7. `Listen` logs, at info level: the endpoint ID; when the relay answered, which one; whether mDNS announces.

**Accept**

8. For every connection the endpoint accepts for the ALPN, a goroutine waits for the client to open its stream (`AcceptStreamConn`), `StreamTimeout` at most. The stream becomes visible only once the client has written to it, which an SSH client does at once. A connection that opens no stream in that time is closed, logged at debug level with the peer's endpoint ID, and never returned by `Accept` (Review Focus 3).
9. `Accept` returns the next connection whose stream is open, as a `net.Conn`:
   - `Read` and `Write` are the stream's. Deadlines work as the stream's do (`SetDeadline`, `SetReadDeadline`, `SetWriteDeadline`): the SSH server sets them.
   - `RemoteAddr` is an `Addr` with the client's endpoint ID; `LocalAddr` an `Addr` with the endpoint's own.
   - **`Close` closes the stream and then, without making its caller wait, the iroh connection**: it returns as soon as the stream is closed, and a goroutine gives the peer up to 2 seconds to close the connection first, so that the last bytes of the stream (an SSH exit status) are not cut off, and then closes it (`CloseWithError`). The probe found that a client often never closes, and that the connection then lingers for QUIC's idle timeout. `Close` may be called more than once and from any goroutine, also while a `Read` is blocked, which then returns. The 2 seconds are a field the tests shorten. `Listener.Close` does not wait for them: it closes every connection at once.
10. A second stream that a client opens on the same connection is not accepted; nothing waits for it.
11. `Accept` blocks until there is a connection, or returns `net.ErrClosed` (so that `errors.Is(err, net.ErrClosed)` holds) once `Close` was called. Connections whose streams opened before `Close` and were not yet accepted are closed by `Close`.
12. The number of connections that are waiting for their stream at once is bounded: at most 64, and one more is closed at once. (The SSH server has its own limit for the handshake, which starts only after `Accept`.)

**Close**

13. `Close` closes the endpoint, every connection it handed out and every connection still waiting, stops the announcing, and returns when the goroutines `Listen` and `Accept` started have ended. It may be called more than once.

**DialLocal**

14. `DialLocal` binds an endpoint of its own with a new key on `127.0.0.1`, without a relay, connects to `id` at `addrs` for the ALPN, opens the stream, and returns it as a `net.Conn` whose `Close` closes the stream, the connection and that endpoint. It is for tests. It does not write: the caller writes first.

## Tests

Every test uses `Local: true` and `DialLocal`. Wait for conditions with a deadline, never with a fixed sleep; where a test needs `StreamTimeout` to pass, make the timeout a field of the listener that the test shortens, with the constant as its default.

| Test | What it shows |
|---|---|
| `TestListenLocal` | an ID of 64 hex characters that is the key's public half; `LocalAddrs` has an address on `127.0.0.1` with a port; `Addr()` is the ID with network `iroh` |
| `TestListenNeedsAWayToBeFound` | not `Local`, `Relay` and `MDNS` both false: an error |
| `TestAcceptHandsOutTheStream` | a client writes `hello`; the accepted connection reads `hello`; what the server writes the client reads; both directions at once |
| `TestRemoteAddrIsTheClientsID` | 64 hex characters, network `iroh`, and not the listener's own ID |
| `TestStreamIsSeenOnlyAfterAWrite` | a client that dialed and wrote nothing: `Accept` has not returned after 200 ms; it returns once the client writes |
| `TestConnectionWithoutAStreamIsClosed` | timeout 100 ms: a client that never writes finds its connection closed; `Accept` never returned it; a client that connects afterwards is served |
| `TestCloseOfAConnClosesTheIrohConnection` | the server closes an accepted connection: the client's `Read` ends with an error or EOF promptly, and a `Write` of the client fails soon after |
| `TestCloseUnblocksRead` | a `Read` blocked on an accepted connection returns when `Close` is called from another goroutine; `Close` twice is fine |
| `TestDeadlines` | `SetReadDeadline` in the past makes `Read` fail with a timeout error (`errors.Is(err, os.ErrDeadlineExceeded)` or a `net.Error` with `Timeout()`), and a later deadline lets it read again |
| `TestClientClosing` | the client closes: the server's `Read` returns EOF or an error, promptly |
| `TestSecondStreamIsIgnored` | a client that opens two streams (dial with go-iroh directly in the test): one connection is accepted, and it works |
| `TestAcceptAfterClose` | `Accept` blocked in a goroutine returns `net.ErrClosed` when the listener is closed; so does a later `Accept` |
| `TestCloseClosesEverything` | accepted connections fail after `Close`; a client still waiting is closed; the goroutines the listener started are gone (count with a deadline) |
| `TestManyClients` | 20 clients at once each get their own connection and their own bytes back |
| `TestWaitingConnectionsAreBounded` | 64 clients that never write, then one more: it is closed at once; after the timeout has closed the others, a new one is served |
| `TestCloseOfAConnGivesThePeerAMoment` | grace 200 ms: a server that writes its last bytes and closes at once: the client still reads them; a client that never closes finds the connection closed about 200 ms later; `Close` itself returned at once |
| `TestAnnouncedAddresses` | made-up interfaces: a LAN interface's addresses are kept; a point-to-point one's, a down one's, a loopback one's and a link-local address are left out; each kept address has the port |
| `TestMDNSPacket` | the packet for one address and for three: the records of 5c, parsed back; the names are z-base-32 of the ID; TTL 0 for the goodbye |
| `TestServesAsANetListener` | `http.Serve(listener, handler)` in a goroutine, and an HTTP request written by hand over a `DialLocal` connection gets its response: the listener is a `net.Listener` in earnest |

What needs the network is not tested here: the relay and mDNS were seen to work by the probe, and are seen again by the end-to-end tests of Task 19.

## Steps

- [ ] `Listen` with `Local`, `DialLocal`, `Accept`, the connection: tests first. Commit: `Irohnet: an endpoint that hands out each connection's stream as a net.Conn`.
- [ ] The timeout, the bound, `Close`: tests first. Commit: `Irohnet: a connection that opens no stream is closed, and Close leaves nothing behind`.
- [ ] The list of addresses (5a) and the mDNS packet (5c): tests first. Commit: `Irohnet: what is announced, and to whom it is no use`.
- [ ] Relay, pkarr and mDNS, from the probe's code. `go build ./...`; run the listener once by hand with `Relay` and `MDNS` on from a small test you delete again, or from a test that is skipped unless `KOSH_NETWORK_TESTS=1`, and see the log say that the relay answered and mDNS announces. Commit: `Irohnet: announced through the relays, number0's DNS and mDNS`.
- [ ] `go test -race ./irohnet/...`; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results; what of the probe's code you carried over unchanged and what you changed; what the log said with the relay and mDNS on; anything in the contract that did not fit.
