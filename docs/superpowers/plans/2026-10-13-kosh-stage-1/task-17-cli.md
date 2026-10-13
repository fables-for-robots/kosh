# Task 17: cli, serve, id and version

The command line, and the wiring of `kosh serve`: everything the other tasks built, put together into the one process that runs in the pod. This is where kosh starts, where it says plainly why it cannot, and where it stops.

**Read first:** the spec, §3, §4, §7, §13, §14 and §18 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); the exported API of what you wire, in the repository as it now is: `state/state.go`, `keys/keys.go`, `irohnet/irohnet.go`, `sshd/sshd.go`, `kube/cluster.go` and `kube/kube.go`, `tui/tui.go`; `cli/cli.go` and `cmd/kosh/main.go` as they stand.

**Files:**
- Modify: `cli/cli.go` (the commands are added to `App`)
- Create: `cli/serve.go`, `cli/id.go`
- Create: `cli/cli_test.go`, `cli/id_test.go`, `cli/serve_test.go`, `cli/serve_envtest_test.go`
- Do not change `cli/pin.go`, `cmd/kosh/main.go`, or any other package.

**Interfaces:**
- Consumes: `state.Load` and its methods; `keys.Open`, `(*keys.List).Watch`; `irohnet.Listen`, `irohnet.Config`; `sshd.New`, `sshd.Config`, `sshd.Session`, `(*sshd.Server).Serve`, `Shutdown`; `kube.RESTConfig`, `kube.New`, `kube.Options`, `(*kube.Cluster).Session`, `Close`; `tui.New`, `tui.Options`; `github.com/urfave/cli/v2`.
- Produces: `cli.App()` with the commands `serve`, `id` and `version`; `cli.Version`.

## Behaviour

**The commands**

1. `kosh version` prints `Version` and a line feed. (`--version` of urfave/cli stays.)
2. `kosh id --state-dir DIR` calls `state.Load(DIR)` and prints exactly these four lines, the labels padded so that the values start in one column:

   ```
   endpoint ID   <64 hex characters>
   iroh-ssh      iroh-ssh kosh@<64 hex characters>
   host key      <ssh-ed25519 AAAA…>
   fingerprint   <SHA256:…>
   ```

   `--state-dir` (variable `KOSH_STATE_DIR`) is required for `id`: without it the command fails with `kosh id: --state-dir is required`. It creates what is missing, as `Load` does.
3. `kosh serve` has the flags of spec §14, each also read from its environment variable: `--ssh-listen` (`KOSH_SSH_LISTEN`, default `:2222`), `--iroh` (`KOSH_IROH`, default true), `--iroh-bind` (`KOSH_IROH_BIND`, default empty: every address, any port), `--iroh-relay` (`KOSH_IROH_RELAY`, default true), `--iroh-mdns` (`KOSH_IROH_MDNS`, default true), `--state-dir` (`KOSH_STATE_DIR`, default `/var/lib/kosh`), `--authorized-keys` (`KOSH_AUTHORIZED_KEYS`, default `/etc/kosh/authorized_keys`), `--kubeconfig` (`KUBECONFIG`, default empty).

**What `serve` checks before it starts anything**

4. `--ssh-listen` empty and `--iroh=false`: `kosh serve: both transports are off: set --ssh-listen or --iroh`.
5. `--iroh` with `--iroh-relay=false` and `--iroh-mdns=false`: `kosh serve: iroh is on and nobody could find it: set --iroh-relay or --iroh-mdns`.
6. `--iroh-bind` that is not empty and does not parse as an address and port (`netip.ParseAddrPort`): an error that names the flag and the value.

**Starting, in this order; each failure is one error that says which step failed and wraps the cause (spec §18)**

7. `state.Load(--state-dir)`.
8. `keys.Open(--authorized-keys, log)`, then `Watch` in a goroutine, every 2 seconds, until the end.
9. `kube.RESTConfig(--kubeconfig)` and `kube.New` with a context of 10 seconds and `kube.Options{Log: log, UserAgent: "kosh/" + Version}`.
10. `sshd.New` with the host key, the key list, the version, the log, and a `Model` that returns `tui.New(cluster.Session(s.Context, s.Grant), tui.Options{Version: Version})`.
11. When `--ssh-listen` is not empty: `net.Listen("tcp", …)`, and `Serve(l, "tcp")` in a goroutine.
12. When `--iroh`: `irohnet.Listen` with a context of 30 seconds and the key, the bind address, `Relay` and `MDNS`, and `Serve(l, "iroh")` in a goroutine.
13. One info line when it serves: message `kosh is serving`, with `version`, `endpoint` (the endpoint ID, when iroh is on), `ssh` (the TCP address it listens on, when SSH is on), `hostkey` (the fingerprint), `cluster` (the API server's version). The endpoint ID in the log is how an operator learns it from `kubectl logs`.
14. What was started before a step that fails is stopped again before `serve` returns the error: no listener, no informer and no goroutine is left behind.

**The log**

15. `log/slog` with a text handler on standard error, at info level. `serve` makes one logger and gives it to everything.

**Stopping (spec §18)**

16. `serve` runs until its context is done (`main` cancels it on SIGINT and SIGTERM), or until a `Serve` returns an error, which then is `serve`'s error.
17. Then, within 10 seconds in all: `server.Shutdown`, the iroh listener's `Close`, the cluster's `Close`. `serve` returns nil after a stop that was asked for. One info line says `kosh has stopped`.

## Tests

`cli/cli_test.go`, `cli/id_test.go`: run `App()` with arguments in the test (`app.Run`), with `app.Writer` and `app.ErrWriter` set to buffers. For `serve`, split the command so that tests reach it: a function that takes the parsed options and a context and returns an error, which the urfave action calls.

| Test | What it shows |
|---|---|
| `TestVersion` | `kosh version` prints `Version` and a line feed |
| `TestIDPrintsFourLines` | on an empty temp directory: four lines with the labels above; the endpoint ID is 64 hex characters and is the same in the first two lines; the host key line parses with `ssh.ParseAuthorizedKey` and its fingerprint is the fourth line's |
| `TestIDIsStable` | twice on the same directory: the same output |
| `TestIDNeedsStateDir` | no flag and no variable: the error above; `KOSH_STATE_DIR` set: it works |
| `TestServeFlagsAndEnvironment` | table: each flag from the command line, and from its variable; the command line wins over the variable; the defaults |
| `TestServeRefusesBothTransportsOff` | the error of 4, and nothing was started (the state directory is still empty) |
| `TestServeRefusesIrohNobodyCanFind` | the error of 5 |
| `TestServeRefusesBadBind` | `--iroh-bind nonsense`: an error with the flag and the value |
| `TestServeFailsOnMissingKeyList` | a state directory, no key list: an error that says the key list could not be read and has the path |
| `TestServeFailsOnUnreachableCluster` | a kubeconfig that points at `https://127.0.0.1:1`: an error within the 10 seconds that says the cluster could not be reached; after it returns, the TCP port `serve` would have used is free and the goroutines it started are gone |

`cli/serve_envtest_test.go` (skipped without `KUBEBUILDER_ASSETS`, as the tests of `kube` are): `serve` whole, in the test's process, against a real API server.

| Test | What it shows |
|---|---|
| `TestServeEndToEnd` | `serve` with a kubeconfig written from the test environment's config, `--ssh-listen 127.0.0.1:0` (the function under test reports the address it got: give it a way), `--iroh=false`, a key list with one full key. An `x/crypto/ssh` client with that key and a terminal of 100 by 30 reads until the screen holds `Pods(default)` and the key's name; it sends `:ns` and `enter` and reads until `Namespaces` and `default` are there; it sends `:q` and `enter` and the session ends with status 0. The log has `kosh is serving` with the cluster's version |
| `TestServeReadOnlyKey` | a `read-only` key: the screen holds `[read-only]` |
| `TestServeUnknownKeyIsRefused` | a key that is not in the list: the handshake fails |
| `TestServeKeyListIsFollowed` | the key's line is removed from the file while its session is open: within 5 seconds the session reads `kosh: this key is no longer accepted` and ends |
| `TestServeStops` | the context cancelled with a session open: the session reads `kosh is shutting down`, `serve` returns nil within 10 seconds, the port is closed, and the log has `kosh has stopped` |
| `TestServeOverIroh` | `--ssh-listen ""`, `--iroh`, with a hook that makes the iroh listener `Local` (an unexported field of the options that the test sets; not a flag): a client over `irohnet.DialLocal` reaches `Pods(default)` |

Reading the screen: the session's output is a stream of escape sequences; strip them (`github.com/charmbracelet/x/ansi`'s `Strip`) from what has been read so far and look for the text, with a deadline. That is enough here; nothing needs a terminal emulator.

## Steps

- [ ] `version` and `id`: tests first. Commit: `CLI: kosh id prints what an operator hands to users`.
- [ ] The flags of `serve` and its three refusals: tests first. Commit: `CLI: the flags of kosh serve, and the settings it refuses`.
- [ ] Starting and stopping, with the failures of 7 to 14: tests first. Commit: `CLI: kosh serve starts what it needs in order, and leaves nothing behind when it cannot`.
- [ ] The API-server tests: `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test -race ./cli/ -run TestServe`. Commit: `CLI: kosh serve, whole, seen from an SSH client`.
- [ ] `go test -race ./...` for the whole module, with and without the assets; `gofmt -l . && go vet ./... && go build ./...` clean. `go build ./cmd/kosh && ./kosh version && ./kosh id --state-dir "$(mktemp -d)"`, then remove the binary.

## Report

What you built and the test results, with and without the API server; anything in the packages you wired that did not behave as its documentation says: name the package, the function, what it did and what you expected. Do not work around such a thing silently; say it.
