# Task 15: sshd

kosh's SSH server: who gets in, and the life of a session. It serves connections from any `net.Listener`, which is how the TCP port and the iroh endpoint come to one server. It faces the network unauthenticated, so its limits and its refusals are the task as much as its sessions are.

**Read first:** the spec, §2, §3, §5, §6, §12, §13 and §18 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the contract: `sshd/sshd.go`; `keys/keys.go` for `List`, `Grant`, `OnChange`; `irohnet/irohnet.go` for `Listen` with `Local` and `DialLocal`.

**The libraries.** `charm.land/wish/v2` (v2.0.5) over `charm.land/ssh` (v0.4.3), which is a fork of gliderlabs/ssh over `golang.org/x/crypto/ssh`. Read `~/go/pkg/mod/charm.land/ssh@v0.4.3/server.go`, `session.go` and `context.go`, and `~/go/pkg/mod/charm.land/wish/v2@v2.0.5/bubbletea/tea.go`, before you design: what follows names their hooks.

**Files:**
- Modify: `sshd/sshd.go` (the bodies, and the fields of `Server`; signatures, constants and documentation stay)
- Create: `sshd/session.go` (a session's life), `sshd/limits.go` (the handshake limit and the keepalive), `sshd/guard.go` (panics)
- Create: `sshd/sshd_test.go`, `sshd/session_test.go`, `sshd/limits_test.go`, `sshd/guard_test.go`, `sshd/iroh_test.go`, `sshd/testhelpers_test.go`

**Interfaces:**
- Consumes: `keys.List` (`Lookup`, `OnChange`), `keys.Grant`; `irohnet.Listen`, `irohnet.DialLocal`, `(*irohnet.Listener).ID`, `LocalAddrs` (tests only); `tea.Model`.
- Produces, exactly as `sshd/sshd.go` declares them: `Config`, `Session`, `Server`, `New`, `(*Server).Serve`, `(*Server).Shutdown`, `(*Server).Sessions`, and the `Msg…` constants.

## Behaviour

**Who gets in**

1. `New` fails when `HostKeyPEM` does not parse, and when `Keys` or `Model` is nil. Zero limits get their defaults (30 s, 64, 30 s, 3).
2. Authentication is by public key and nothing else: the public key handler accepts a key when `cfg.Keys.Lookup` finds it at that moment. No password handler, no keyboard-interactive handler. The user name is not looked at.
3. The server's version string is `SSH-2.0-kosh_<Version>`.
4. **The handshake has a deadline** of `HandshakeTimeout` (`Server.HandshakeTimeout` of the library). A connection that has not authenticated by then is closed: one that sends nothing, one that sends bytes that are not SSH, one that asks whether a key would do and then stalls (Review Focus 3).
5. **At most `MaxHandshakes` connections are in the handshake at once**, over every listener together. A connection that arrives when they are all taken is closed at once, and logged. A connection leaves the handshake when it has authenticated, or when it is closed, whichever is first, and never twice.

   "Has authenticated" is the point: x/crypto's server calls the public key callback already when a client only *asks* whether a key would be accepted, before it has proved anything, so "the handler said yes" is not it. `gossh.ServerConfig.AuthLogCallback` is called with a nil error once a client has authenticated; the library lets you set it through `ServerConfigCallback`. To know which connection it is for, the `net.Conn` you hand the library (through `ConnCallback`, or by wrapping before `HandleConn`) can carry its slot: for example a wrapper whose `RemoteAddr` returns a value of a type of your own that holds the real address and the slot, which `AuthLogCallback` finds through `ConnMetadata.RemoteAddr()`. Make sure whatever you do leaves `RemoteAddr().String()` the real address, for the log.

**Sessions**

6. A session that asks for a command (`ssh host cmd`): `MsgNoCommands` and a line feed on its error stream, exit status 1. This is checked before the terminal.
7. A session without a terminal: `MsgNeedsTerminal`, exit status 1.
8. A session with a terminal: the grant is looked up again (the list may have changed since the handshake; a key that is gone gets `MsgKeyRemoved`, status 1). Then `cfg.Model` is called with the `Session`, and its model is run as a Bubble Tea program on the session's terminal with the options of wish's `bubbletea.MakeOptions`. When the program ends by itself the session exits with status 0.
9. `Session.Context` is done when the session ends, however it ends. `Session.Transport` is what `Serve` was told; `Session.Remote` is the connection's remote address as a string.
10. Nothing else is served: a `direct-tcpip` channel is refused, a `tcpip-forward` request is refused, a subsystem request fails, agent forwarding is not acted on. Environment requests are accepted. These are the library's defaults when no handler is set; your tests show it.
11. `Sessions()` is the number of sessions with a running program.

**Ending a session from kosh's side**

12. One function ends a session with a line: it stops the program, and when the program has stopped and the terminal is back to its normal screen, writes the line and a line feed to the session and exits it with status 1. Every case below uses it.
13. **The key list changed** (`cfg.Keys.OnChange`): every open session's key is looked up. One that is no longer found is ended with `MsgKeyRemoved`; one whose `ReadOnly` differs from the grant the session began with is ended with `MsgGrantChanged`. A change of the name alone ends nothing. The function you register must not block the caller of `Replace` on sessions: collect what to end under your lock, end them outside it.
14. **`Shutdown`** closes every listener `Serve` was given, ends every session with `MsgShuttingDown`, then shuts the library's server down, and returns when every connection is closed or its context is done. `Serve` returns nil then, and also when its listener is closed.
15. **A panic** in the model's `Init`, `Update` or `View` is recovered: it is logged at error level with the key's name and the stack, the session is ended with `MsgCrashed`, and every other session goes on. Wrap the model for this (`sshd/guard.go`); let Bubble Tea keep catching what it catches itself, so that a panic in a command's goroutine cannot take the process down, and log that case too when the program returns `tea.ErrProgramPanic`.

**Keepalive**

16. For every session, the server sends the request `keepalive@openssh.com` with want-reply on the session's connection every `KeepaliveInterval`. Any reply, success or failure, is an answer. A request that has no reply within the interval is a miss; `KeepaliveMisses` misses in a row close the connection. `SendRequest` blocks until the reply: call it from a goroutine of its own so that a dead peer cannot hold the keepalive loop. One loop per connection, not per session, and it ends with the connection.

**The log (spec §13)**, with `cfg.Log`, one line per event, as attributes:

17. A connection that is closed without authenticating: transport, remote, reason (the library's `ConnectionFailedCallback`; the handshake limit; the deadline).
18. A session's start: transport, remote, key name, fingerprint, read-only. Its end: the same, how long it lasted, and why when kosh ended it.
19. The library logs through `slog.Default()` on its own in places; that is left alone.

## Tests

`sshd/testhelpers_test.go`: a server on a loopback TCP listener with a `keys.List` the test controls, a generated host key and client keys, short limits; a client with `golang.org/x/crypto/ssh` (`HostKeyCallback: ssh.InsecureIgnoreHostKey()` is right in a test); a helper that opens a session with a terminal (`RequestPty("xterm-256color", 24, 80, …)`, `Shell()`) and reads its output until a text appears, with a deadline; a test model that draws a fixed word and the session's grant, quits on `q`, and can be told to panic. Wait for conditions with a deadline, never with a fixed sleep. After each test, the goroutines the server started are gone (check with a deadline; this is what shows that nothing leaks).

| Test | What it shows |
|---|---|
| `TestNewValidates` | a bad host key, nil `Keys`, nil `Model`: errors |
| `TestAcceptedKeyGetsTheProgram` | the test model's word arrives; `Sessions()` is 1; `q` ends the session with status 0 and `Sessions()` is 0 |
| `TestRejectedKey` | a key that is not in the list: the handshake fails; a line in the log with the remote and the reason |
| `TestNoPasswordAuth` | a client that offers a password is refused, and the server offers only `publickey` |
| `TestAnyUserName` | users `root`, `kosh` and `x` all get in with the key |
| `TestVersionString` | the client sees `SSH-2.0-kosh_<Version>` |
| `TestSessionGetsItsGrant` | a read-only key: `Session.Grant.ReadOnly` is true in the model; `Transport` is `tcp`; `Remote` is the client's address |
| `TestNoTerminal` | `Shell()` without `RequestPty`: `MsgNeedsTerminal` on the session's error stream, status 1 |
| `TestCommandIsRefused` | `Run("ls")`, with and without a terminal: `MsgNoCommands`, status 1; the model was not made |
| `TestNothingElseIsServed` | a `direct-tcpip` channel is rejected; `tcpip-forward` is refused; a `subsystem sftp` request fails |
| `TestContextEndsWithTheSession` | `Session.Context` is done after the client closes, and after `q` |
| `TestKeyRemovedEndsTheSession` | `Replace` with a list without the key: the client reads `MsgKeyRemoved`, status 1; a second session of another key stays open |
| `TestGrantChangedEndsTheSession` | full to read-only, and back: `MsgGrantChanged`. Only the name changed: the session stays |
| `TestKeyRemovedBetweenHandshakeAndSession` | the key is removed after the connection authenticated and before the session is opened: `MsgKeyRemoved` |
| `TestReplaceDoesNotBlockOnSessions` | `Replace` returns within 100 ms while a session's client is not reading |
| `TestShutdown` | two sessions: both read `MsgShuttingDown`; `Shutdown` and `Serve` return; a new connection is refused |
| `TestPanicInUpdateEndsOnlyThatSession` | the model of one session panics in `Update`: its client reads `MsgCrashed`, the log has the stack and the key's name, the other session still answers |
| `TestPanicInViewAndInit` | the same for `View` and for `Init` |
| `TestSilentPeerIsClosedByTheDeadline` | a TCP connection that sends nothing, `HandshakeTimeout` 100 ms: closed by the server soon after; the log names the remote |
| `TestGarbagePeerIsClosed` | a connection that sends `GET / HTTP/1.1\r\n\r\n`, and one that sends 1 MB of zeros: closed, and the server goes on serving |
| `TestHandshakeLimit` | `MaxHandshakes` 3: three idle connections are held; a fourth is closed at once; after one of the three authenticates, another idle one is held again; after the deadline has closed the idle ones, three more are held |
| `TestAskingAboutAKeyDoesNotLeaveTheHandshake` | a client that sends the public key query for an accepted key and then stalls still counts against the limit, and is closed by the deadline. (Drive the query with `x/crypto/ssh`'s client using a signer whose `Sign` blocks: the client library asks first and signs after.) |
| `TestKeepaliveClosesADeadPeer` | interval 20 ms, misses 3: a client whose connection stops answering requests (a `net.Conn` wrapper in the test that swallows what the server sends from a moment on) is closed by the server; `Session.Context` is done |
| `TestKeepaliveLeavesALivePeer` | the `x/crypto/ssh` client answers by itself: the session is still open after 20 intervals |
| `TestServeTwoListeners` | two TCP listeners with transports `a` and `b`: sessions from each see their own `Transport`; closing one listener leaves the other serving |

`sshd/iroh_test.go`, the same server over iroh: `irohnet.Listen` with `Local: true`, served with transport `iroh`; a client connection from `irohnet.DialLocal(ctx, l.ID(), l.LocalAddrs())` given to `ssh.NewClientConn`.

| Test | What it shows |
|---|---|
| `TestIrohAcceptedKey` | the test model's word arrives over iroh; `Session.Transport` is `iroh` and `Session.Remote` is 64 hex characters |
| `TestIrohRejectedKey` | the handshake fails |
| `TestIrohSilentPeer` | a connection that opens its stream by writing one byte that is not SSH and then nothing: closed by the handshake deadline |

## Steps

- [ ] The helpers and the test model; `New`; `Serve` with authentication and the program. Tests first. Commit: `SSHD: an accepted key gets its program, and nothing else is served`.
- [ ] Ending a session; the key list; `Shutdown`. Commit: `SSHD: a session ends when its key goes, when its grant changes, and when kosh stops`.
- [ ] The guard. Commit: `SSHD: a panic ends its own session and no other`.
- [ ] The handshake deadline and limit. Commit: `SSHD: a peer that does not authenticate is closed by a deadline, and only so many may try at once`.
- [ ] The keepalive. Commit: `SSHD: a peer that stopped answering is closed`.
- [ ] Over iroh. Commit: `SSHD: the same server over an iroh endpoint`.
- [ ] `go test -race ./sshd/...`; `gofmt -l . && go vet ./... && go build ./...` clean.

## Report

What you built and the test results; how the handshake limit knows that a connection has authenticated, and what you verified about `AuthLogCallback` in the library versions at hand; anything of the library that forced a departure from the behaviour above; anything in the contract that did not fit.
