# Task 20: closing

What is left when every other task is merged: the scaffolding comes out, the whole is checked as a whole, and the branch is reviewed as one change.

**Files:**
- Delete: `cli/pin.go`
- Modify: `go.mod`, `go.sum` (by `go mod tidy`, now that every dependency is imported for real)
- Create: `render/all_test.go`
- Modify: `docs/superpowers/plans/2026-10-13-kosh-stage-1.md` (a table of departures), and create `docs/superpowers/plans/2026-10-13-kosh-stage-1/ledger.md`

## Steps

- [ ] **The pins.** Delete `cli/pin.go`. Run `go mod tidy`. `git diff go.mod`: every module that left was a pin nothing imports; say which, and why nothing imports it. `go build ./...`, `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...`, `go vet ./...`.

- [ ] **Every kind has its renderer.** `render/all_test.go`:

  ```go
  package render

  import "testing"

  // The finished program has a renderer for each of the 26 kinds, and
  // for no other.
  func TestEveryKindHasARenderer(t *testing.T) {
  	got := map[string]bool{}
  	for _, gvr := range Kinds() {
  		got[gvr.String()] = true
  	}
  	for _, gvr := range All {
  		if !got[gvr.String()] {
  			t.Errorf("no renderer for %s", gvr)
  		}
  		delete(got, gvr.String())
  	}
  	for gvr := range got {
  		t.Errorf("a renderer for %s, which is not in All", gvr)
  	}
  }
  ```

- [ ] **No placeholder is left.** `grep -rn 'not implemented' --include='*.go' .` finds nothing. `grep -rn 'internal/' --include='*.go' . | grep -v '_test.go' | grep fables-for-robots/kosh` finds nothing: no package of kosh is under `internal/`. No `os/exec` outside tests: `grep -rln '"os/exec"' --include='*.go' . | grep -v _test.go` finds nothing.

- [ ] **The whole suite**, twice, with the race detector and the API server: `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test -race -count=2 ./...`. A test that fails once in two runs is a finding, not a retry.

- [ ] **The scripts:** `scripts/test-chart.sh`; on Linux, the image and `scripts/test-image.sh`, and the end-to-end test of Task 19.

- [ ] Commit: `Closing: the pins are out, and every kind is seen to have its renderer`.

- [ ] **The ledger and the departures.** `ledger.md` beside the task files holds what the execution's working ledger held: each task's commits, what its review found and what was done, every ruling, every deferred finding. The plan's main file gets a section "Departures" with a table: what the plan said, what was built, why. Commit: `Docs: the plan of stage 1 as it was carried out, and the ledger of its execution`.

- [ ] **The whole branch is reviewed** as one change, against the spec, by a reviewer who has seen none of it, with the deferred findings of the ledger to triage. What it finds that must be fixed is fixed in one wave and reviewed once more.
