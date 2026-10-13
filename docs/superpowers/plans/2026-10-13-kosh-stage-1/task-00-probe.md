# Task 0: the probe, iroh-ssh against go-iroh

Spec §20 asked for two things to be tried with a throwaway program before anything of kosh was built: that the real `iroh-ssh` reaches a go-iroh endpoint, by the relays and by mDNS alone, and that a Bubble Tea program runs over that stream as it does over TCP. This is the report of that probe, as its author wrote it, followed by what the plan takes from it.

## What the plan takes from it

- **The relay path holds** for the released `iroh-ssh` and for its git head. `irohnet` announces as the probe's `endpoint.go` does: default relays, a pkarr record with number0's DNS through `iroh.N0PkarrPublisher` with an address filter of its own, republished when the addresses change.
- **mDNS needs an announcer of kosh's own.** go-iroh's `iroh/mdns` cannot be found by `iroh-ssh`: its names are the ID in RFC 4648 base32 where `iroh-ssh` asks for z-base-32, and it sends no TXT record, which `iroh-ssh`'s resolver waits for. `irohnet` carries the probe's `peermdns.go`, and runs it alone: `iroh-ssh` is the only client kosh has.
- **Only the git head of `iroh-ssh` looks anything up by mDNS.** The released 0.2.12 is built on iroh 0.94 and has no mDNS. `--iroh-mdns` therefore serves users of a build from git, until a release has it.
- **No address of a tunnel is announced**, in mDNS or in pkarr: only addresses of interfaces that are up, broadcast, multicast and not point-to-point. A tunnel's address in the announcement made connections time out.
- **The server ends the connection.** A client that exits often sends no close, and the SSH connection then lingers until QUIC's idle timeout, half a minute. So `sshd` closes a connection that has had no session for 5 seconds (Task 15), and `irohnet`'s `Close` of a connection closes the stream, gives the peer up to 2 seconds to close first, so that the last bytes are not cut off, and then closes the iroh connection, without blocking its caller (Task 4).
- **`AcceptStreamConn` returns only when the client has written**, as expected: the deadline of `StreamTimeout` stands.
- **A peer's address on the relay path is a placeholder.** The endpoint ID is what identifies it, which is what `irohnet.Addr` holds.
- **`iroh-ssh` lets `ssh` offer the agent's keys before the one named with `-i`.** `sshd` allows 20 authentication attempts instead of the library's 6 (Task 15).
- **A Bubble Tea program over the stream behaves as over TCP**: first frame, keys, resizing, colours, a clean exit.

The probe's source is in the session's scratchpad and is not part of the repository; the four files are reproduced in the report below.

---

# kosh feasibility probe: report

## Verdict

| | Result | Short version |
|---|---|---|
| Q1 relay path | **PASS** | `probe-ok` with both clients, including when the pkarr record carries the relay alone. |
| Q2 mDNS only | **FAIL with go-iroh's `iroh/mdns`; PASS with a ~200-line announcer of my own** | Two record-layout mismatches; a third problem is which addresses you announce. |
| Q3 Bubble Tea over the stream | **PASS** | All five checks pass on iroh and on TCP; the difference is in teardown, not in the session. |

Four things you need to know before building on this:

1. **"iroh-ssh v0.2.12" is two different programs.** The released one (GitHub macOS asset, crates.io) is built on iroh 0.94 and has no mDNS or DHT lookup at all. Only git HEAD (`82e9971`, 2026-10-10, still versioned 0.2.12) pins iroh 1.3.0 with `iroh-mdns-peer-lookup`. mDNS discovery therefore only matters for users who build from git.
2. **go-iroh's mDNS cannot be found by iroh-ssh.** It needs z-base-32 names and a mandatory TXT record; code below.
3. **Tunnel-interface addresses break connections on this Mac.** A go-iroh endpoint cannot complete a handshake arriving at its own Tailscale (`utun6`) address, and announcing that address over mDNS made every iroh-ssh connection time out.
4. **The server cannot rely on the client to end the connection.** After a clean exit, `HandleConn` often returns only at the 30–35 s QUIC idle timeout; closing the SSH connection when the session ends fixes that.

One incident: the whole `probe/` directory was deleted twice (about 15:01 and 15:12 local) by something else using the shared scratchpad. I rewrote it from memory each time and re-ran tests; details under "What is on disk".

## Clients used

- **Released binary**: `https://github.com/rustonbsd/iroh-ssh/releases/download/0.2.12/iroh-ssh.macos`, sha256 `064094cd96d51d1dfedecec37d01138dc521c69739ddc19b66367a7e318bca4a`. It is x86_64 only and runs under Rosetta on this arm64 Mac.
  - `strings` shows `iroh-0.94.0`, `hickory-proto-0.25.2`, no `irohv1`; as a server it never opens UDP 5353.
  - crates.io `iroh-ssh 0.2.12` likewise depends on `iroh ^0.94`.
- **Git HEAD build**: built from the cloned repo with nix's rustc 1.98.1, because the local cargo 1.86 is too old (hick needs 1.91). Native arm64, about 2 minutes.
  - I deleted this binary at the end, following your CLAUDE.md rule on generated binaries. To rebuild (P = the probe dir, R = the cloned repo):
    `CARGO_TARGET_DIR=$P/cargo-target nix shell nixpkgs#cargo nixpkgs#rustc -c cargo install --path $R --locked --root $P/iroh-ssh-git-bin`
  - The scripts expect it at `probe/iroh-ssh-git-bin/bin/iroh-ssh`.

## Q1 — relay path: PASS

Server: `./probe --mdns=false` (and `--direct=false` for the relay-only record). Client command, run for both clients (`scripts/q1.sh`, `DIRECT=false scripts/q1.sh`):

```
iroh-ssh -i testkey -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null x@<id>   </dev/null
```

Output, identical for both clients and both record types:

```
Pseudo-terminal will not be allocated because stdin is not a terminal.
Warning: Permanently added '9b4bd1de…c249' (ED25519) to the list of known hosts.
probe-ok
exit=0
```

- With direct addresses in the record, the server accepted from `remoteAddr=192.168.1.92:<port>` (direct path); total client time 0.13 s (git) and 0.33–0.63 s (release).
- With the relay alone in the record, the server accepted from `remoteAddr=[fd15:70a:510b:1::1]:12345`, a placeholder address for the relay path; client time 1.4 s.
- Relay start-up: `ep.Online()` returned in 0.49–0.57 s on every start.
- About 3 s after start the home relay changed (`aps1-1` to `euc1-1` or `usw1-1`) and the pkarr record was republished. A connection made 2.5 s after that still worked.

## Q2 — mDNS only

### go-iroh's `iroh/mdns`: FAIL

`./probe --relay=false --mdns-impl=goiroh`, git client, 3 of 3 attempts (run twice), each after 5 s:

```
Error: No addressing information available
    1: All address lookup services failed or produced no results
           Service 'pkarr' failed: Error resolving http request
           Service 'dns' failed: no calls succeeded: [Failed to resolve TXT record…]
```

Cause, from a packet capture on 224.0.0.251:5353 and from reading both sides:

- **Names differ.** The client asks for `<z-base-32 id>._irohv1._udp.local.` and `<z-base-32 id>.local.`; go-iroh announces the id in RFC 4648 base32. Captured for one id: the client asked for `sb8ewe4yxzidhsf8…ayno`, go-iroh announced `wbhiui2apxvd4wfh…yacq`.
- **TXT is mandatory.** The client's resolver (`hick` 0.2.0 `resolve_instance`) completes only when it holds SRV, TXT and at least one address (`Builder::complete` in `hick-reactor`). go-iroh sends no TXT unless there is a relay URL or user data.
- The client sends four separate questions (SRV, TXT for the instance; A, AAAA for the host), plain class IN, repeated at +1 s and +3 s, with a 5 s budget.
- The Rust side drops responses whose source port is not 5353 (`mdns-proto` `endpoint/receive.rs`).
- The reference Rust announcer sends PTR (TTL 30), SRV and TXT with the cache-flush bit, one A record for the numerically first address, the rest as TXT `a1=`, `a2=` (address bytes + port, big endian), plus an NSEC.

The released 0.94 client fails differently and in 0.4 s (`Discovery produced no results`), because it never asks mDNS.

### With `peermdns.go`: PASS

`./probe --relay=false` (default `--mdns-impl=peer`), git client, `scripts/q2.sh`:

```
== probe --relay=false --mdns-impl=peer   id=c6186ce4…8540
probe-ok  [0 s]
probe-ok  [2 s]
probe-ok  [1 s]
== probe --relay=false --mdns-impl=goiroh   id=2d51f9f0…cd95
Error: No addressing information available  Caused by:  [5 s]   (x3)
== probe --relay=false --mdns-impl=both   id=223be519…b5a7
probe-ok  [0 s]
probe-ok  [2 s]
probe-ok  [1 s]
```

- Same result on a second run of the script; `scripts/repeat.sh --relay=false` gave 12 of 12.
- Running both announcers together works: both bind 5353 with `SO_REUSEPORT` and use different instance names.
- Server log for the passing case: `peer mdns announcing instance=aacg33fj…oiyy._irohv1._udp.local. addrs="[192.168.1.92:54943 10.241.247.228:54943]" interfaces=6`.
- Same-machine testing works: the client's multicast query is looped back to the server's socket and the reply back to the client.

### The address problem

With all four interface addresses announced (`192.168.1.92`, `10.241.247.228`, Tailscale `100.79.132.116`, Tailscale `fd7a:…`), every connection timed out after 40 s (`Error: timed out`), 7 of 7, although lookup succeeded. A plain UDP socket announced the same way received the client's 1200-byte QUIC Initials on all three IPv4 addresses, so discovery was fine and the handshake was not.

| Addresses announced over mDNS | Result |
|---|---|
| LAN only (`192.168.1.92`) | pass |
| LAN + `10.241.247.228` (feth), either order | pass |
| LAN + Tailscale IPv4 | fail |
| LAN + Tailscale IPv6 | pass |
| `10.241.247.228` alone | pass |
| Tailscale IPv4 alone | fail |
| Tailscale IPv6 alone | fail |
| `10.241.247.228` + Tailscale IPv4 | fail |

- A go-iroh client dialling a go-iroh server in one process shows the same thing without any Rust or mDNS: `en0` connects in 3 ms, `feth3832` in 1 ms, both `utun6` addresses time out at 5 s. A plain UDP echo to `100.79.132.116` works. I did not find the cause inside go-iroh.
- Through pkarr alone the same full address set works (26 of 26 over `--mdns=false` runs); a pkarr record with only the Tailscale address fails for both clients.
- With the full default config (pkarr with all addresses plus mDNS with LAN addresses), 5 of 40 git-client connections timed out, all on a fresh server (first or second attempt; one right after an earlier timeout). With `PROBE_ADDRS=0,1` (no tunnel address anywhere) 44 of 44 passed. That points at the tunnel address but the samples are small; I did not prove it.
- Not tested: whether a client on another machine reaches go-iroh through a real Tailscale tunnel.

The probe now announces over mDNS only addresses of interfaces that are up, broadcast, multicast and not point-to-point. It still puts every address in pkarr, as jaccard-store does; given the above I would leave tunnel addresses out there too.

## Q3 — Bubble Tea over the stream: PASS

`scripts/q3.sh` starts `./probe` with defaults and runs `scripts/ptydrive.py` (pty of 100x30, `TERM=xterm-256color`) against each transport. Output for the git client over iroh:

```
PASS  first frame shows pty size 'size 100x30'  (124 ms)  t+0.12s since spawn
  screen now: Warning: Permanently added '7bd5cfb9…8c8f' (ED25519)… | size 100x30 | key (none) | coloured line
PASS  key press shows 'key a'  (17 ms)
PASS  resize to 120x40 shows 'size 120x40'  (16 ms)
  screen now: size 120x40 | key a | coloured line
PASS  ANSI colour sequences present  (0 ms)  SGR seen: '\x1b[38;5;205;1m', '\x1b[m'
PASS  'q' ends the session with exit status 0  (13 ms)  exit status 0
  last bytes: …\x1b[2A\x1b[4Cq\x1b[>4m\x1b[<1u\r\n\r\n\x1b[J\x1b[?25h\x1b[?2004lConnection to 7bd5cfb9…8c8f closed.\r\r\n
```

| | TCP `ssh -tt -p 22220` | iroh, git client | iroh, released client |
|---|---|---|---|
| First frame after spawn | 33–38 ms | 106–124 ms | 178–196 ms |
| Key shown | 16–17 ms | 17 ms | 17 ms |
| Resize shown | 17 ms | 16 ms | 17–20 ms |
| Colour | `38;5;205;1` | same | same |
| `q`, exit status | 0 | 0 | 0 |
| Bytes received | 335 | 437 | 437 |

- No behavioural difference in the session. 94 of the 102 extra bytes are the longer host name in two ssh messages; I did not diff the remaining 8, and the raw captures were lost in the deletion.
- The resize was done with TIOCSWINSZ on the pty master plus SIGWINCH to the child pid. Under `iroh-ssh -t` the child is the wrapper, not `ssh`; the ioctl signals the whole foreground group, which is what reaches `ssh`. I did not test the two separately.
- Sessions log `pty=true emulated=true`: wish's default is an emulated PTY, and the middleware takes the colour profile from the client's `TERM`. `#ff5faf` came out as 256-colour `38;5;205`.
- Bubble Tea v2 repaints cell by cell (`\x1bM\x1b[9Da\x1b[K` for the key change), so "key a" never appears literally in the byte stream. The script uses a small VT emulator for that reason.
- Further pty runs with the rebuilt code: 12 of 12 sessions passed all five checks (`scripts/teardown.sh`, both clients).

## Other observations

**Client disappears mid-session** (SIGKILL to the client's process group after the first frame):
- TCP: the session ended 19 ms later.
- iroh: the session and its Bubble Tea program ran on for 35 s, until `iroh conn context done … cause="timeout: no recent network activity"`. Nothing else tells the server.
- go-iroh's defaults are a 30 s idle timeout on the relay path and a 5 s heartbeat; `iroh.WithTransportConfig` can change them (untested).

**Clean exit also lingers.** Without intervention, of 14 iroh connections that ended normally, 8 returned from `HandleConn` only after 30–35 s and 6 within 10–76 ms. Both clients do it, with and without a pty: `ssh` kills its ProxyCommand on exit, and whether the proxy gets its FIN out first is a race. The git HEAD commit "FIN packet now sent" does not remove it.

**Fix tested: `--close-after-session`.** The outermost middleware calls `s.Exit(0)` and then closes the `gossh` connection from the session context. 16 of 16 sessions (both clients, pty and no pty) kept `rc=0` and full output, and `HandleConn` returned in 10–103 ms every time. It closes the whole connection, so it would break several sessions multiplexed on one connection (ControlMaster).

**The server must close the iroh connection itself.** `HandleConn` closes only the stream (go-iroh's `streamConn.Close` sends FIN and cancels reading). In the last run 13 of 16 connections were still open 2 s later and were closed by the server's `CloseWithError`. I wait up to 2 s for the peer before closing, so as not to cut off the exit status; I did not test closing immediately.

**`AcceptStreamConn` waits for the client's first bytes.** With a client that connected and stayed silent (`iroh-ssh proxy <id>:22` with a quiet stdin), the server logged `iroh conn accepted` and then `stream accepted after=1.702s`, exactly when the client finally wrote its version line; the server's own banner went out only after that. With a real `ssh` it is 0–2 ms direct and 31–90 ms through the relay. A client that never writes parks a goroutine there, so give that call a deadline.

## Surprises and gotchas

- The Go toolchain is 1.26.8 by auto-download: go-iroh needs 1.26, wish 1.26.8, and the `go` on PATH is 1.23.12.
- go-iroh's default relays are `*.relay.n0.iroh-canary.iroh.link`; a neighbouring Rust node on this LAN advertises `euc1-1.relay.n0.iroh.link`. Both clients still connected through the canary relay named in the pkarr record.
- `conn.RemoteAddr()` on the relay path is the placeholder `[fd15:70a:510b:1::N]:12345`; do not use it for logging identity or rate limits. `conn.RemoteID()` is the stable thing.
- iroh-ssh does not pass `IdentitiesOnly`, so `ssh` offers agent keys first: the public-key callback saw `ok=false` then `ok=true` on every iroh login. A user with many agent keys could hit the auth-attempt limit before `-i` is tried.
- The publisher's default address filter publishes the relay alone; `AddrFilter: publishableAddrs` is needed to get direct addresses into pkarr (as in jaccard-store).
- A failed iroh 1.3.0 dial takes 40 s before `ssh` reports `Connection closed by UNKNOWN port 65535`.
- Another iroh node on this LAN (192.168.1.61) browses `_irohv1._udp.local.` PTR about once a second. An announcer that answers every browse multicasts constantly; mine answers a browse at most once a second and a burst of instance questions once per 50 ms.
- `keygen.New("", keygen.WithKeyType(keygen.Ed25519))` without `WithWrite` gives an in-memory host key; feed `RawPrivateKey()` to `wish.WithHostKeyPEM`.
- Side effects outside `probe/`: Go modules in `~/go/pkg/mod`, crates in `~/.cargo/registry`, rustc and cargo in the nix store. Nothing was installed globally.

## Module versions

Go 1.26.8 darwin/arm64. `require` block of `probe/server/go.mod`:

```
require (
	charm.land/bubbletea/v2 v2.0.10
	charm.land/lipgloss/v2 v2.0.6
	charm.land/ssh v0.4.3
	charm.land/wish/v2 v2.0.5
	github.com/charmbracelet/keygen v0.5.4
	github.com/tmc/go-iroh v0.3.0
	golang.org/x/net v0.61.0
	golang.org/x/sys v0.49.0
)
```

Indirect versions that resolved with them include `golang.org/x/crypto v0.58.0`, `charm.land/log/v2 v2.0.1`, `github.com/charmbracelet/colorprofile v0.4.3`, `github.com/charmbracelet/x/ansi v0.11.8`, `github.com/quic-go/quic-go v0.62.0`. The first build, before the directory was deleted, had resolved `x/crypto v0.57.0` and `x/sys v0.48.0`; results were the same.

## Code

All four files are `package main` in `probe/server/`, exactly as on disk.

### `main.go` — flags, accept loop, stream to `HandleConn`

```go
// Command probe is throwaway code: an SSH server (wish v2) reachable over an
// iroh endpoint with ALPN /iroh/ssh, to test compatibility with iroh-ssh.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tmc/go-iroh/iroh"
	irohkey "github.com/tmc/go-iroh/key"
)

const alpn = "/iroh/ssh"

func main() {
	useRelay := flag.Bool("relay", true, "default relays and the N0 pkarr publisher")
	useMDNS := flag.Bool("mdns", true, "announce over mDNS")
	mdnsImpl := flag.String("mdns-impl", "peer", "peer: the announcer in peermdns.go, which iroh-ssh can resolve; goiroh: go-iroh's iroh/mdns, which it cannot; both")
	pkarr := flag.Bool("pkarr", true, "with --relay: publish the pkarr record (false: relay in use, but nothing published)")
	direct := flag.Bool("direct", true, "advertise the interface addresses (false: pkarr carries the relay alone)")
	closeAfter := flag.Bool("close-after-session", false, "close the SSH connection when its session ends")
	authKeys := flag.String("authorized-key", "../testkey.pub", "the one public key that may log in")
	tcpAddr := flag.String("tcp", "127.0.0.1:22220", "TCP control listener")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sk, err := irohkey.GenerateSecretKey()
	if err != nil {
		fatal(log, "key", err)
	}
	srv, err := newSSHServer(*authKeys, *closeAfter, log)
	if err != nil {
		fatal(log, "ssh server", err)
	}

	t0 := time.Now()
	ep, err := bindEndpoint(ctx, sk, *useRelay, *pkarr, *useMDNS, *direct, *mdnsImpl, log)
	if err != nil {
		fatal(log, "bind", err)
	}
	log.Info("endpoint ready", "took", time.Since(t0).Round(time.Millisecond), "local", ep.LocalAddr())
	fmt.Printf("ENDPOINT_ID %s\n", ep.ID())

	ln, err := net.Listen("tcp", *tcpAddr)
	if err != nil {
		fatal(log, "tcp listen", err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && ctx.Err() == nil {
			log.Error("tcp serve", "error", err)
		}
	}()

	go acceptLoop(ctx, ep, srv, log)

	<-ctx.Done()
	_ = srv.Close()
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = ep.Shutdown(sctx)
}

// acceptLoop accepts iroh connections; each carries one bidirectional stream
// of raw SSH, which is handed to the SSH server as a net.Conn.
func acceptLoop(ctx context.Context, ep *iroh.Endpoint, srv sshServer, log *slog.Logger) {
	for {
		conn, err := ep.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("accept", "error", err)
			return
		}
		go serveConn(ctx, conn, srv, log)
	}
}

func serveConn(ctx context.Context, conn *iroh.Conn, srv sshServer, log *slog.Logger) {
	log = log.With("remote", conn.RemoteID().Short())
	t0 := time.Now()
	log.Info("iroh conn accepted", "alpn", conn.ALPN(), "remoteAddr", conn.RemoteAddr())
	go func() {
		<-conn.Context().Done()
		log.Info("iroh conn context done", "after", time.Since(t0).Round(time.Millisecond), "cause", context.Cause(conn.Context()))
	}()

	// Returns once the client has sent its first bytes on the stream (the
	// SSH version line): a QUIC stream does not exist for the acceptor
	// before that.
	stream, err := conn.AcceptStreamConn(ctx)
	if err != nil {
		log.Error("accept stream", "error", err)
		_ = conn.CloseWithError(1, "no stream")
		return
	}
	log.Info("stream accepted", "after", time.Since(t0).Round(time.Millisecond))

	srv.HandleConn(stream) // returns when the SSH connection is over
	log.Info("HandleConn returned", "after", time.Since(t0).Round(time.Millisecond))

	// HandleConn has closed the stream (FIN on our side, read cancelled). The
	// QUIC connection is still open, and closing it at once could drop the
	// last bytes of the stream, so give the peer a moment to go first.
	select {
	case <-conn.Context().Done():
	case <-time.After(2 * time.Second):
		log.Info("peer did not close within grace; closing")
	}
	_ = conn.CloseWithError(0, "bye")
}

func fatal(log *slog.Logger, what string, err error) {
	log.Error(what, "error", err)
	os.Exit(1)
}
```

### `endpoint.go` — bind and announce, with the relay and mDNS switches

`PROBE_ADDRS` and `PROBE_PKARR_IP_ONLY` are experiment hooks; drop them.

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tmc/go-iroh/dns"
	"github.com/tmc/go-iroh/iroh"
	"github.com/tmc/go-iroh/iroh/mdns"
	irohkey "github.com/tmc/go-iroh/key"
	"github.com/tmc/go-iroh/netaddr"
	"github.com/tmc/go-iroh/relay"
)

// bindEndpoint binds the endpoint and announces it. With useRelay it uses the
// default relays and (with usePkarr) publishes a pkarr record with number0;
// without, it uses no relay and publishes nothing. With useMDNS it announces
// the LAN addresses on the local link.
func bindEndpoint(ctx context.Context, sk irohkey.SecretKey, useRelay, usePkarr, useMDNS, advertise bool, mdnsImpl string, log *slog.Logger) (*iroh.Endpoint, error) {
	opts := []iroh.Option{iroh.WithSecretKey(sk), iroh.WithALPNs(alpn)}
	if useRelay {
		opts = append(opts, iroh.WithRelayMode(relay.ModeDefault()))
	} else {
		opts = append(opts, iroh.WithRelayMode(relay.ModeDisabled()))
	}
	ep, err := iroh.Bind(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("bind: %w", err)
	}

	if useRelay {
		t0 := time.Now()
		octx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := ep.Online(octx)
		cancel()
		log.Info("relay online wait finished", "took", time.Since(t0).Round(time.Millisecond), "error", err)
	}

	// The socket is bound to the wildcard address; what a peer can dial are
	// the interface addresses on the bound port.
	var direct []netip.AddrPort
	if advertise {
		direct = selectAddrs(interfaceAddrs(ep.LocalAddr().Port(), false))
	}
	for _, ap := range direct {
		ep.AddExternalAddr(ap)
	}

	actx, cancel := context.WithCancel(ctx)
	go func() {
		defer cancel()
		select {
		case <-ep.Closed():
		case <-ctx.Done():
		}
	}()

	// mDNS reaches the local link only, so it announces only the addresses
	// of LAN interfaces. (On the test machine the address of a Tailscale
	// tunnel interface in the announcement made every connection from
	// iroh-ssh time out, although a LAN address was announced with it.)
	var lan []netip.AddrPort
	var lanAddrs []netaddr.TransportAddr
	if advertise {
		lan = selectAddrs(interfaceAddrs(ep.LocalAddr().Port(), true))
		for _, ap := range lan {
			lanAddrs = append(lanAddrs, netaddr.IPAddr{Addr: ap})
		}
	}
	log.Info("direct addresses", "all", direct, "lan", lan)
	if useMDNS && len(lan) > 0 {
		if mdnsImpl == "goiroh" || mdnsImpl == "both" {
			// For go-iroh clients and Rust clients of iroh-mdns-address-lookup.
			local := mdns.New(ep.ID(), mdns.WithLogger(log))
			local.Publish(dns.NewEndpointData(lanAddrs...))
			go func() {
				if err := local.Start(actx); err != nil && actx.Err() == nil {
					log.Warn("go-iroh mDNS unavailable", "error", err)
				}
			}()
		}
		if mdnsImpl == "peer" || mdnsImpl == "both" {
			// For iroh-ssh, i.e. Rust clients of iroh-mdns-peer-lookup.
			go func() {
				if err := runPeerMDNS(actx, ep.ID(), lan, log); err != nil && actx.Err() == nil {
					log.Warn("peer mDNS unavailable", "error", err)
				}
			}()
		}
	}

	if useRelay && usePkarr {
		pub, err := iroh.N0PkarrPublisher(sk, &iroh.PkarrPublisherConfig{AddrFilter: publishableAddrs})
		if err != nil {
			_ = ep.Shutdown(context.Background())
			return nil, fmt.Errorf("pkarr publisher: %w", err)
		}
		go func() {
			defer pub.Close()
			// Republished whenever the home relay or the addresses change.
			for addr := range ep.WatchAddr().Stream(actx) {
				addrs := publishableAddrs(addr.Addrs())
				if !advertise {
					addrs = keep[netaddr.RelayAddr](addrs)
				}
				if os.Getenv("PROBE_PKARR_IP_ONLY") != "" { // experiment: no relay in the record
					addrs = keep[netaddr.IPAddr](addrs)
				}
				pub.Publish(dns.NewEndpointData(addrs...))
				log.Info("publishing pkarr record", "addrs", addrs)
			}
		}()
	}
	return ep, nil
}

// keep returns the addresses of type T.
func keep[T netaddr.TransportAddr](addrs []netaddr.TransportAddr) []netaddr.TransportAddr {
	var out []netaddr.TransportAddr
	for _, a := range addrs {
		if _, ok := a.(T); ok {
			out = append(out, a)
		}
	}
	return out
}

// selectAddrs is an experiment hook: PROBE_ADDRS="0,2" keeps those entries.
func selectAddrs(all []netip.AddrPort) []netip.AddrPort {
	sel := os.Getenv("PROBE_ADDRS")
	if sel == "" {
		return all
	}
	var out []netip.AddrPort
	for _, s := range strings.Split(sel, ",") {
		if i, err := strconv.Atoi(s); err == nil && i < len(all) {
			out = append(out, all[i])
		}
	}
	return out
}

// interfaceAddrs pairs the machine's unicast interface addresses with port.
// With lanOnly it keeps only interfaces that broadcast and multicast and are
// not point-to-point: no tunnels (utun, tailscale, wireguard).
func interfaceAddrs(port uint16, lanOnly bool) []netip.AddrPort {
	var out []netip.AddrPort
	seen := map[netip.Addr]bool{}
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		if lanOnly && (ifc.Flags&net.FlagPointToPoint != 0 || ifc.Flags&net.FlagBroadcast == 0 || ifc.Flags&net.FlagMulticast == 0) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
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

// publishableAddrs keeps every address but invalid and unspecified IPs. The
// publisher's default filter would publish the relay alone.
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
```

### `sshd.go` — wish server, Bubble Tea middleware, the model

```go
package main

import (
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"charm.land/ssh"
	"charm.land/wish/v2"
	bm "charm.land/wish/v2/bubbletea"
	"github.com/charmbracelet/keygen"
)

// sshServer is what the accept loop needs of the SSH server.
type sshServer interface {
	HandleConn(net.Conn)
}

// newSSHServer builds the wish server: a generated in-memory host key,
// public-key auth for exactly the key in authKeyPath, any user name.
func newSSHServer(authKeyPath string, closeAfterSession bool, log *slog.Logger) (*ssh.Server, error) {
	raw, err := os.ReadFile(authKeyPath)
	if err != nil {
		return nil, err
	}
	allowed, _, _, _, err := ssh.ParseAuthorizedKey(raw)
	if err != nil {
		return nil, fmt.Errorf("authorized key: %w", err)
	}

	// keygen.New without WithWrite generates a key and writes nothing.
	host, err := keygen.New("", keygen.WithKeyType(keygen.Ed25519))
	if err != nil {
		return nil, fmt.Errorf("host key: %w", err)
	}

	return wish.NewServer(
		wish.WithHostKeyPEM(host.RawPrivateKey()),
		wish.WithPublicKeyAuth(func(ctx ssh.Context, k ssh.PublicKey) bool {
			ok := ssh.KeysEqual(k, allowed)
			log.Info("public key auth", "user", ctx.User(), "ok", ok, "remote", ctx.RemoteAddr())
			return ok
		}),
		wish.WithMiddleware(
			// Middlewares run last to first: logging, then the no-PTY
			// answer, then the Bubble Tea program.
			bm.Middleware(func(s ssh.Session) (tea.Model, []tea.ProgramOption) {
				pty, _, _ := s.Pty()
				return model{w: pty.Window.Width, h: pty.Window.Height}, nil
			}),
			func(next ssh.Handler) ssh.Handler {
				return func(s ssh.Session) {
					if _, _, ok := s.Pty(); !ok {
						_, _ = io.WriteString(s, "probe-ok\n")
						_ = s.Exit(0)
						return
					}
					next(s)
				}
			},
			func(next ssh.Handler) ssh.Handler {
				return func(s ssh.Session) {
					_, _, pty := s.Pty()
					log.Info("session start", "user", s.User(), "pty", pty, "emulated", s.EmulatedPty(), "remote", s.RemoteAddr())
					next(s)
					log.Info("session end", "user", s.User())
					if closeAfterSession {
						// A client that is killed (ssh kills its ProxyCommand)
						// sends no QUIC close, so over iroh nothing ends the
						// SSH connection until the idle timeout. End it here:
						// send the exit status, then close the connection.
						_ = s.Exit(0) // an error means the handler already exited
						if c, ok := s.Context().Value(ssh.ContextKeyConn).(io.Closer); ok {
							_ = c.Close()
						}
					}
				}
			},
		),
	)
}

// The Bubble Tea program of a PTY session.

var accent = lipgloss.NewStyle().Foreground(lipgloss.Color("#ff5faf")).Bold(true)

type model struct {
	w, h int
	key  string
}

func (m model) Init() tea.Cmd { return nil }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tea.KeyPressMsg:
		m.key = msg.String()
		if m.key == "q" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m model) View() tea.View {
	key := m.key
	if key == "" {
		key = "(none)"
	}
	return tea.NewView(fmt.Sprintf("size %dx%d\nkey %s\n%s\n", m.w, m.h, key, accent.Render("coloured line")))
}
```

### `peermdns.go` — the Q2 change

Why it is needed: go-iroh's `iroh/mdns` has no option for the label encoding or for forcing a TXT record (`WithServiceName` changes only the service), so patching it would mean changing both name construction and the announcement builder. A separate announcer is smaller, and it can run beside go-iroh's.

```go
package main

// An mDNS announcer that the Rust crate iroh-mdns-peer-lookup 0.1.2 (the one
// iroh-ssh uses) can resolve. go-iroh's iroh/mdns cannot be used for this:
// see the comment on runPeerMDNS.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"syscall"
	"time"

	irohkey "github.com/tmc/go-iroh/key"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const (
	peerService = "_irohv1._udp.local."
	peerTTL     = 120
	// classFlush is class IN with the mDNS cache-flush bit: the record set
	// replaces what a listener has cached for the name.
	classFlush = dnsmessage.ClassINET | 1<<15
)

var mdnsGroup4 = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// runPeerMDNS announces the endpoint id at addrs the way
// iroh-mdns-peer-lookup does, and answers its queries, until ctx is done.
//
// What that crate's resolver (hick's resolve_instance) asks for and needs:
//
//	SRV  <z32 id>._irohv1._udp.local.  -> port, target <z32 id>.local.
//	TXT  <z32 id>._irohv1._udp.local.  -> must be present, even if empty
//	A    <z32 id>.local.               (AAAA too)
//
// The dialable addresses are (every A/AAAA of the target) x (the SRV port),
// plus one address per TXT segment "a<N>=" followed by the 4 or 16 address
// bytes and the port, big endian. The lookup returns at the first moment it
// holds SRV, TXT and one address, so only the TXT segments are sure to
// arrive together; this announcer therefore puts every address but the first
// in TXT, as the Rust announcer does, and the first in A/AAAA.
//
// go-iroh's iroh/mdns differs in two ways that each make the lookup fail:
// its instance and host labels are the id in RFC 4648 base32 where the crate
// uses z-base-32, so the names never match; and it sends no TXT record
// unless there is a relay URL or user data, and the resolver does not
// complete without one.
//
// Responses are sent from port 5353: the Rust side drops responses from any
// other source port.
func runPeerMDNS(ctx context.Context, id irohkey.EndpointID, addrs []netip.AddrPort, log *slog.Logger) error {
	if len(addrs) == 0 {
		return errors.New("peer mdns: no addresses")
	}
	z32 := id.Z32()
	instance := z32 + "." + peerService
	host := z32 + ".local."

	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			if serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); serr != nil {
				return
			}
			serr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", "0.0.0.0:5353")
	if err != nil {
		return fmt.Errorf("peer mdns: listen: %w", err)
	}
	defer pc.Close()
	p := ipv4.NewPacketConn(pc)
	_ = p.SetMulticastLoopback(true) // clients on this machine must hear us
	_ = p.SetMulticastTTL(255)

	// Join, and later send, on every LAN interface: up, multicast, and not a
	// tunnel. Loopback of our own multicast serves clients on this machine.
	var ifaces []net.Interface
	all, _ := net.Interfaces()
	for i := range all {
		if all[i].Flags&net.FlagUp == 0 || all[i].Flags&net.FlagMulticast == 0 ||
			all[i].Flags&net.FlagPointToPoint != 0 || all[i].Flags&net.FlagLoopback != 0 {
			continue
		}
		if err := p.JoinGroup(&all[i], mdnsGroup4); err == nil {
			ifaces = append(ifaces, all[i])
		}
	}
	if len(ifaces) == 0 {
		return errors.New("peer mdns: no multicast interface")
	}

	announcement, err := peerPacket(instance, host, addrs, peerTTL)
	if err != nil {
		return err
	}
	goodbye, err := peerPacket(instance, host, addrs, 0)
	if err != nil {
		return err
	}
	send := func(packet []byte) {
		for i := range ifaces {
			if err := p.SetMulticastInterface(&ifaces[i]); err != nil {
				continue
			}
			_, _ = pc.WriteTo(packet, mdnsGroup4)
		}
	}

	go func() {
		// Unsolicited announcements, then silence until asked.
		for _, wait := range []time.Duration{0, time.Second, 2 * time.Second} {
			select {
			case <-ctx.Done():
			case <-time.After(wait):
				send(announcement)
				continue
			}
			break
		}
		<-ctx.Done()
		send(goodbye)
		_ = pc.Close()
	}()
	log.Info("peer mdns announcing", "instance", instance, "addrs", addrs, "interfaces", len(ifaces))

	// One response carries every record, so it answers every question about
	// this endpoint at once. The resolver sends its four questions (SRV, TXT,
	// A, AAAA) in four packets within a few milliseconds; answer the burst
	// once. A browse of the whole service (PTR) is answered at most once a
	// second, as RFC 6762 section 6 asks for multicast responses.
	const burst, browseInterval = 50 * time.Millisecond, time.Second
	var lastSent time.Time
	browse, own := strings.ToLower(peerService), []string{strings.ToLower(instance), strings.ToLower(host)}
	buf := make([]byte, 9000)
	for {
		n, src, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("peer mdns: read: %w", err)
		}
		var parser dnsmessage.Parser
		hdr, err := parser.Start(buf[:n])
		if err != nil || hdr.Response {
			continue
		}
		questions, err := parser.AllQuestions()
		if err != nil {
			continue
		}
		wait := time.Duration(-1) // no question of ours
		for _, q := range questions {
			qname := strings.ToLower(q.Name.String())
			switch {
			case qname == own[0] || qname == own[1]:
				wait = burst
			case qname == browse && wait < 0:
				wait = browseInterval
			}
		}
		if wait < 0 || time.Since(lastSent) < wait {
			continue
		}
		lastSent = time.Now()
		log.Debug("peer mdns answering", "from", src, "question", questions[0].Name.String(), "type", questions[0].Type)
		send(announcement)
	}
}

// peerPacket builds the response carrying the whole record set. Everything
// is in the answer section, so that it stands as an announcement as well as
// a reply to any one of the questions.
func peerPacket(instance, host string, addrs []netip.AddrPort, ttl uint32) ([]byte, error) {
	svcName, err := dnsmessage.NewName(peerService)
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
	// TXT: every address after the first, "a<N>=" + address bytes + port.
	// With none, a TXT record holding one empty string, which DNS-SD
	// requires anyway and without which the Rust resolver never completes.
	var txt []string
	for i, ap := range addrs[1:] {
		ip := ap.Addr().Unmap()
		seg := append([]byte(fmt.Sprintf("a%d=", i+1)), ip.AsSlice()...)
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
```

Untested parts of the announcer: IPv6 multicast (the Rust crate looks up over IPv4 only by default), a machine with no LAN interface up, address changes after start (it announces the set it was given once), and a client on a second machine.

## What is on disk

Everything is under `/private/tmp/claude-502/-Users-dragan-fables-for-robots/25996946-215e-4a11-ba9e-574cacaa75eb/scratchpad/probe/`:

- `server/main.go`, `server/endpoint.go`, `server/peermdns.go`, `server/sshd.go`, `server/go.mod`, `server/go.sum`
- `scripts/ptydrive.py` (the pty driver), `scripts/q1.sh`, `scripts/q2.sh`, `scripts/q3.sh`, `scripts/teardown.sh`, `scripts/repeat.sh`
- `testkey`, `testkey.pub` (ed25519, no passphrase)
- `iroh-ssh-bin/iroh-ssh` — the released 0.2.12 macOS binary (x86_64, iroh 0.94, no mDNS)
- `td.log`, `td.out` — server log of the last `--close-after-session` run

Because of the two deletions:
- The directory was rewritten twice, and the test key is the third one generated.
- Q1 and Q2 were re-run after the first rewrite. The teardown and silent-client tests ran after the second, whose files are identical apart from merging the TUI model into `sshd.go` and dropping the `--trap` flag.
- The standalone Q3 run (`scripts/q3.sh`) predates the first deletion.
- Not on disk any more: the raw pty captures, the mDNS packet logs, the downloaded hick/mdns-proto sources, and three diagnostic tools (an mDNS sniffer, the UDP "trap" announcer, the go-iroh self-dial test).
- If the directory is missing again when you look, the code above is complete.

To run anything, build first: `cd probe/server && go build -o probe .`; the scripts start `./probe` themselves. Scripts other than the TCP half of `q3.sh` also need the git client rebuilt with the command under "Clients used".

No process of mine is running. The Go probe binary, the cargo target directory and the git-built iroh-ssh are deleted.
