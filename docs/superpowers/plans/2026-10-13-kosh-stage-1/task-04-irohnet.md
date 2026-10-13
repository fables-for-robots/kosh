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
5. `cfg.MDNS`: the endpoint is announced on the local network in the way the probe found `iroh-ssh` to find it. mDNS that cannot be had (no multicast, the port taken) is a warning in the log, and the listener works without it.
6. The context bounds `Listen` alone. What `Listen` starts lives until `Close`.
7. `Listen` logs, at info level: the endpoint ID; when the relay answered, which one; whether mDNS announces.

**Accept**

8. For every connection the endpoint accepts for the ALPN, a goroutine waits for the client to open its stream (`AcceptStreamConn`), `StreamTimeout` at most. The stream becomes visible only once the client has written to it, which an SSH client does at once. A connection that opens no stream in that time is closed, logged at debug level with the peer's endpoint ID, and never returned by `Accept` (Review Focus 3).
9. `Accept` returns the next connection whose stream is open, as a `net.Conn`:
   - `Read` and `Write` are the stream's. Deadlines work as the stream's do (`SetDeadline`, `SetReadDeadline`, `SetWriteDeadline`): the SSH server sets them.
   - `RemoteAddr` is an `Addr` with the client's endpoint ID; `LocalAddr` an `Addr` with the endpoint's own.
   - **`Close` closes the stream and then the iroh connection**, so that nothing of a finished SSH connection is left; it may be called more than once and from any goroutine, also while a `Read` is blocked, which then returns.
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
| `TestServesAsANetListener` | `http.Serve(listener, handler)` in a goroutine, and an HTTP request written by hand over a `DialLocal` connection gets its response: the listener is a `net.Listener` in earnest |

What needs the network is not tested here: the relay and mDNS were seen to work by the probe, and are seen again by the end-to-end tests of Task 19.

## Steps

- [ ] `Listen` with `Local`, `DialLocal`, `Accept`, the connection: tests first. Commit: `Irohnet: an endpoint that hands out each connection's stream as a net.Conn`.
- [ ] The timeout, the bound, `Close`: tests first. Commit: `Irohnet: a connection that opens no stream is closed, and Close leaves nothing behind`.
- [ ] Relay, pkarr and mDNS, from the probe's code. `go build ./...`; run the listener once by hand with `Relay` and `MDNS` on from a small test you delete again, or from a test that is skipped unless `KOSH_NETWORK_TESTS=1`, and see the log say that the relay answered and mDNS announces. Commit: `Irohnet: announced through the relays, number0's DNS and mDNS`.
- [ ] `go test -race ./irohnet/...`; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results; what of the probe's code you carried over unchanged and what you changed; what the log said with the relay and mDNS on; anything in the contract that did not fit.
