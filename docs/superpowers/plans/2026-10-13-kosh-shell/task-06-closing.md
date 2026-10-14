# Task 6: closing

- [ ] **The whole suite**, twice, with the race detector and the API server: `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test -race -count=2 ./...`, and once shuffled. A test that fails once is a finding.
- [ ] **The module:** `go mod tidy -diff` empty; `go vet ./...` and `GOOS=linux go vet -tags e2e ./e2e/`; the build for linux/amd64 without cgo; no `os/exec` outside tests; no package under `internal/`.
- [ ] **The feature is reviewed as a whole**, against the spec, by a reviewer who has seen none of it, with the security of the boundary as the first question: can anyone who is not a full key get a shell, by any path; can a shell outlive what the spec says it may; what one key can make kosh hold through shells; what reaches the log.
- [ ] What the review finds that must be fixed is fixed in one wave and reviewed once more.
- [ ] **The ledger** of the execution beside the task files (`ledger.md`), and a section "Departures" in the plan's main file.
- [ ] A pull request, with CI green.
