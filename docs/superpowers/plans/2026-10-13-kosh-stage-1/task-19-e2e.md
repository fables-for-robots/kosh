# Task 19: end to end, and the README

kosh as its users meet it: the image in a real cluster, installed with the chart, reached with the real `ssh` and the real `iroh-ssh`. And the README, which is what a person reads to get there.

**Read first:** the spec, §1, §15, §19 and §20 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); what the probe learned about `iroh-ssh` (`docs/superpowers/plans/2026-10-13-kosh-stage-1/task-00-probe.md`: "Clients used", "Q1", "Q2", "Surprises and gotchas", and its `scripts/ptydrive.py` approach to driving a terminal); `charts/kosh/values.yaml` and `scripts/test-chart.sh`; `scripts/test-image.sh` and `flake.nix`; `cli/serve_envtest_test.go` for how a test reads kosh's screen through an SSH client.

**Where this runs.** On Linux, with Docker, Nix and the network. This Mac cannot run it whole. Use the x86_64 Linux machine `framework.ffr` (`ssh framework.ffr`; it has Nix and Docker and no system Go): copy the commit there with `git archive HEAD | ssh framework.ffr 'mkdir -p /tmp/kosh-e2e && tar -x -C /tmp/kosh-e2e'`, keep everything you make there under that one directory (Go's caches too: `GOCACHE`, `GOMODCACHE`, `GOFLAGS=-modcacherw`), and when you are done delete the kind cluster, the images you loaded and the directory. If the sandbox blocks `ssh` or the network, run those commands with the sandbox disabled.

**Files:**
- Create: `scripts/test-e2e.sh`, `scripts/test-mdns.sh`
- Create: `e2e/e2e_test.go`, `e2e/screen_test.go`, `e2e/doc.go` (Go tests behind the build tag `e2e`)
- Create: `e2e/testdata/` for the manifests the tests apply
- Modify: `.github/workflows/test.yml` (one job added), `flake.nix` only if the development shell lacks a tool the scripts need
- Replace: `README.md`
- Do not change any other Go package, the chart, or the other scripts. If the end-to-end test shows a defect in one of them, that is this task's most valuable result: report it with the evidence, and do not fix it here.

**Interfaces:**
- Consumes: the image (`nix build .#image`), the chart, `kosh id`, the log line `kosh is serving` with `endpoint=<id>`, the UI as a user sees it.
- Produces: `scripts/test-e2e.sh` and `scripts/test-mdns.sh`, each exiting 0 only when everything they check held.

## The end-to-end test

**`scripts/test-e2e.sh`** does, in order, and cleans up after itself whatever happens (a `trap`):

1. Builds the image (`nix build .#image` with the evaluation option, `docker load`), unless `KOSH_E2E_IMAGE` names one that is already loaded.
2. Creates a kind cluster of its own, named `kosh-e2e-<random>`, with a kubeconfig file of its own, and loads the image into it. It never touches another cluster or the user's kubeconfig.
3. Makes two client keys with `ssh-keygen`, a full one and a read-only one, and the state: `docker run --rm -v <dir>:/state IMAGE id --state-dir /state`, then a Secret `kosh-state` from that directory in the namespace `kosh`.
4. Installs the chart with Helm: the image, `image.pullPolicy=Never`, `state.existingSecret=kosh-state`, `authorizedKeys` holding the full key and the read-only key with `read-only` in front, `iroh.mdns=false`. Waits for the rollout.
5. Reads the endpoint ID from the pod's log (`kosh is serving … endpoint=…`) and checks it is the one `kosh id` printed in step 3.
6. Applies `e2e/testdata/`: a pod `marker` in `default` that prints `kosh-e2e-marker` every second (a busybox image, loaded into kind beforehand so that nothing is pulled mid-test), a deployment `web` of one replica of the pause image, a Secret `hidden` with the value `kosh-e2e-secret-value`, and a CRD `widgets.e2e.kosh.test` with one instance. Waits for the pod to run.
7. Forwards a local port to the Service (`kubectl port-forward svc/kosh 0:22`, the port read from its output).
8. Runs `go test -tags e2e -count=1 ./e2e/` with what the tests need in the environment: the SSH address, the two key files, the kubeconfig, the endpoint ID, and the path of `iroh-ssh`.
9. Deletes the cluster and its temporary files.

**`iroh-ssh` for the test** is the released 0.2.12 binary for Linux x86_64 from the project's GitHub release, fetched by the script and checked against a SHA-256 written in the script: it is what users have, and the probe saw it work over the relays. Look at the release's assets for the name of the Linux one. If there is none for the machine at hand, the script builds the pinned commit `82e9971` with the command the probe's report gives, and says so.

**`e2e/e2e_test.go`**, with the build tag `e2e`. Every test reads kosh's screen as a user would see it: bytes from a terminal session, kept in a small screen model. Bubble Tea repaints cell by cell, so searching the raw bytes for a word fails (the probe found this); `e2e/screen_test.go` holds a minimal terminal emulator for exactly what kosh emits, or uses `github.com/charmbracelet/x/vt` if it is in `go.mod` (it is not to be added). Each wait has a deadline of 30 seconds and, when it fails, prints the screen it had.

| Test | What it shows |
|---|---|
| `TestSSHShowsThePods` | a session with the full key over the forwarded port, terminal 120 by 40: the screen shows `Pods(default)`, the row of `marker` with STATUS `Running`, the key's name in the header, and the cluster's version |
| `TestSSHLogs` | on `marker`, `l`: the screen shows `kosh-e2e-marker`, and a new line of it within 5 seconds (the log is followed) |
| `TestSSHDescribeAndYAML` | `d` shows `Name:` and `marker`; `esc`; `y` shows `kind: Pod` |
| `TestSSHNavigates` | `:deploy` shows `web`; `enter` shows its pod; `esc` returns; `:ns` shows `default` and `kosh`; `:widgets` shows the instance, in the API server's table |
| `TestSSHScales` | in deployments, `s`, the field set to `3`, the dialog confirmed: `kubectl get deploy web -o jsonpath={.spec.replicas}` is `3` within 10 seconds, and kosh's log has an `action` line with the full key's name, `scale` and `deployments/default/web` |
| `TestSSHKillAsksFirst` | on a pod of `web`, `ctrl+k` then `enter` at once (the focus is on Cancel): the pod still exists; `ctrl+k`, `→`, `enter`: it is deleted |
| `TestReadOnlyKey` | the read-only key: the header shows `[read-only]`; in deployments `s` opens nothing and the replica count is unchanged after it; in secrets, the row of `hidden` is there, and `y`, `d` and `x` each leave the table shown and the screen says `this key is read-only`; `kosh-e2e-secret-value` never appears on the screen; the log has a `refused` line |
| `TestUnknownKeyIsRefused` | a third key that is not in the list: the handshake fails |
| `TestNoCommandNoShell` | `ssh … ls` gets `kosh runs no commands` and status 1 (run the real `ssh` binary for this one, with `-T`); `ssh -T` without a command gets `kosh needs a terminal` |
| `TestKeyRemovedEndsTheSession` | with a session of the read-only key open, `helm upgrade` with `authorizedKeys` holding only the full key: within 120 seconds (the kubelet's own delay in updating a mounted ConfigMap) the session ends with `this key is no longer accepted`, and the full key's session, opened before, is still alive |
| `TestIrohSSH` | the real `iroh-ssh -i <full key> -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes kosh@<endpoint ID>` started on a pseudo-terminal of 120 by 40 (`github.com/creack/pty`, which is in `go.mod`): the screen shows `Pods(default)` and `marker`; `:q` and `enter` end it with status 0. This goes through number0's DNS and a relay: give it 60 seconds, and when it fails print kosh's log beside the screen. Skipped, saying so, when `KOSH_E2E_NO_NETWORK=1` |
| `TestIrohSSHReadOnly` | the same with the read-only key: `[read-only]` |

**`scripts/test-mdns.sh`** shows that `iroh-ssh` finds kosh by mDNS alone. It needs no cluster of its own beyond an API to talk to, and it needs a build of `iroh-ssh` from its repository at `82e9971`, because the released one has no mDNS: it builds that with Nix's cargo into a cache directory (the probe's report has the command; it takes a few minutes the first time), runs `kosh serve` as a process on the machine, built from the tree with `go build`, with `--iroh-relay=false`, `--ssh-listen ""`, a kubeconfig of a kind cluster it creates (or `KUBECONFIG` given to it), a temporary state directory and key list, and then runs `iroh-ssh` on the same machine on a pseudo-terminal until the screen shows `Pods(default)`. It removes what it built and started. It is run by hand and by the workflow's job; it is not part of `test-e2e.sh`, because it needs neither the image nor the chart.

**The workflow.** `test.yml` gets a job `e2e` on `ubuntu-latest`, after the job that builds the image: Nix installed as the other jobs do it, `scripts/test-e2e.sh`, then `scripts/test-mdns.sh`. Multicast between two processes of one runner works on the loopback; if GitHub's runner turns out not to deliver it, say so in your report and let the job run `test-mdns.sh` with `continue-on-error`, with a comment that says why.

## The README

`README.md` is for someone who has never heard of kosh and has a cluster. In this order, each section short:

1. **What it is**, in three sentences, and a screenshot's worth of text: a frame of the pods table as kosh draws it (take it from a golden file of the UI's tests).
2. **Install**: the state Secret made with `kosh id` from the image (`docker run`), the Helm command with `authorizedKeys`, where to find the endpoint ID.
3. **Connect**: with `ssh` (a port-forward for a first look; `service.type=LoadBalancer` to expose it), and with `iroh-ssh kosh@<endpoint ID>`, with a sentence on what iroh buys (no address to expose, nothing to open) and that the SSH host key is checked by `ssh` as always. That `iroh-ssh`'s released build finds kosh through the relays, and a build from its repository also by mDNS on the same network when kosh runs on the host network.
4. **Keys**: the file's format, `read-only`, what a read-only key cannot do (no action, no Secret's contents) and what it still can (everything else the ServiceAccount may read), that the list is followed without a restart, that a certificate is not accepted and a comment may not begin with `read-only`.
5. **What a key can do**: a full key is whatever the ServiceAccount is, `cluster-admin` by default; how to bind something narrower (`rbac.clusterRole`), and that kosh then shows the API's refusal where it lacks a right. That Kubernetes' audit log names kosh, and kosh's own log names the key.
6. **Using it**: the keys that matter (`:` `/` `enter` `esc` `?` `d` `y` `l` `e` `ctrl-d` `ctrl-k`, the namespace digits), and that `?` lists the rest. One line on what differs from k9s (edit is built in, copy goes to your clipboard through the terminal, kill asks).
7. **Values**: the table of the chart's values, from `values.yaml`.
8. **Not yet**: a shell in a pod, port-forward, metrics; a line each.
9. **Development**: `direnv allow` or `nix develop`, `go test ./...`, the API-server tests and their assets script, `scripts/test-chart.sh`, `scripts/test-e2e.sh`, and where the design and the plan are.
10. **Licence**: LGPL-3.0-only; the credit to k9s, with a pointer to `NOTICE`.

Every command in the README is one you ran. No claim in it is about something the end-to-end test did not show or the spec does not say.

## Steps

- [ ] `e2e/screen_test.go` with a unit test of the screen model that runs without the tag's environment (a recorded byte stream from one of the UI's golden tests, or a hand-written one: cursor moves, erase in line, SGR ignored, a line overwritten cell by cell).
- [ ] `scripts/test-e2e.sh` up to the chart installed and the endpoint ID read, on `framework.ffr`. Commit: `E2E: kosh installed with its chart into a kind cluster`.
- [ ] The SSH tests, one at a time, each seen to fail for the right reason first (run it before the thing it asserts is set up, or assert the opposite once). Commit: `E2E: the UI seen through ssh: the pods, a log, an action, and a read-only key`.
- [ ] The key list changed, and the refusals. Commit: `E2E: a key that leaves the list ends its session`.
- [ ] `iroh-ssh` over the relays. Commit: `E2E: the same over iroh-ssh`.
- [ ] `scripts/test-mdns.sh`. Commit: `E2E: iroh-ssh finds kosh by mDNS alone`.
- [ ] The workflow's job. The README. Commit: `README: what kosh is, how to install it and how to get in`.
- [ ] Both scripts pass from a clean checkout on `framework.ffr`, twice in a row. Nothing is left there, and nothing here.

## Report

The output of both scripts' last runs; every defect the tests showed in kosh itself, with the screen or the log that shows it and the package it is in, whether or not it stopped you; what you had to work around to make a test pass, which is as much a finding as a failure; anything of the README you could not verify.
