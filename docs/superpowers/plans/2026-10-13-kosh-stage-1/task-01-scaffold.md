# Task 1: scaffold and contract

The module, its pinned dependencies, and the contract of every package as code: the exported types, interfaces and function signatures, documented, with bodies that panic `not implemented` where a later task fills them in. After this task the whole module builds, and every later task works behind signatures that the compiler holds everyone to.

This task was carried out by the author of the plan, in a scratch module first, so that the contract was compiled, and its fake tested, before any task was handed out.

**Files:**

| File | What it holds |
|---|---|
| `go.mod`, `go.sum` | every dependency of the plan, at the versions that were seen to build together, also for `linux/amd64` without cgo |
| `cli/pin.go` | behind the build tag `pin`: a blank import of every package a later task uses, which is what keeps them in `go.mod` through `go mod tidy`. Task 20 removes it |
| `cmd/kosh/main.go` | `main`: the signals, `cli.App` |
| `cli/cli.go` | `Version`, `App` with no commands yet (Task 17) |
| `keys/keys.go` | `Grant`, `Rejected`, `Set`, `Parse`, `List`, `NewList`, `Open`, `Watch`, `Lookup`, `Replace`, `OnChange` (Task 2) |
| `state/state.go` | `State`, `Load`, `EndpointID`, `HostKeyLine`, `HostKeyFingerprint` (Task 3) |
| `irohnet/irohnet.go` | `ALPN`, `Config`, `Listener` as a `net.Listener`, `Addr`, `Listen`, `DialLocal` (Task 4) |
| `sshd/sshd.go` | `Config`, `Session`, `Server`, `New`, `Serve`, `Shutdown`, the lines a session is told (Task 15) |
| `render/render.go` | `Health`, `Column`, `Row`, `Renderer`, `Register`, `For`, `Zero`, `Kinds`: complete |
| `render/cell.go` | `B`, the builder of a row, and the blank values: complete |
| `render/kinds.go` | the GVR of each of the 26 typed kinds, and `All`: complete |
| `render/container.go`, `render/rule.go` | `ContainerColumns`, `ContainerRows` (Task 5); `RuleColumns`, `RuleRows` (Task 7) |
| `render/render_test.go`, `render/helpers_test.go` | the tests every renderer must pass, and the helpers every renderer's tests use: complete |
| `kube/kube.go` | `Resource`, `ObjectRef`, `Query`, `Table`, `Watch`, the log types, the options of delete and drain, `Reader`, `Actions`, `Session`, the errors: complete |
| `kube/cluster.go` | `Options`, `Cluster`, `RESTConfig`, `New`, `Session`, `Close` (Task 8) |
| `kube/kubetest/kubetest.go` | the fake `Session` of the UI's tests, with its own tests: complete |
| `tui/tui.go` | `Options`, `New` (Task 11) |
| `tui/view.go` | `view`, `filterable`, `dialog`, the messages to the root, `env`, `binding`, `bind`, `bindingsFor`: complete |
| `LICENSE`, `COPYING`, `NOTICE`, `README.md`, `.gitignore` | |

**Verified:** `go build ./...`, `go vet ./...`, `go build -tags pin ./...`, `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -tags pin ./...`, and `go test ./...` (the fake and the renderers' shared tests) pass.
