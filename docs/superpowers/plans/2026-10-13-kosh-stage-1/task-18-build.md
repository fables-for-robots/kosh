# Task 18: the flake, the image and the workflows

How kosh is built outside a developer's `go build`: by Nix, into a program and an image, and by GitHub's workflows, which test every push and publish a release.

**Read first:** the spec, §15 and §16 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`).

**The model** is the sibling project great-machine, read-only at `/Users/dragan/fables-for-robots/great-machine`: its `flake.nix`, `.envrc`, `.gitignore`, `.github/workflows/test.yml` and `.github/workflows/release.yml`. Read all five with their comments before you write anything: kosh's are the same in kind and simpler, and the comments there say why things are as they are (gonixgo and its evaluation option, the version from the environment, an image without store paths, building a Linux image from a Mac). Where this brief is silent, do as great-machine does.

**Files:**
- Create: `flake.nix`, `flake.lock` (by `nix flake lock`), `.envrc`
- Create: `.github/workflows/test.yml`, `.github/workflows/release.yml`
- Create: `scripts/test-image.sh`
- Modify: `.gitignore` only if something you add needs it
- Do not change any Go file, the chart, or the other scripts.

**Where to verify.** This Mac builds the program for Darwin. The image is a Linux image: build and run it on the x86_64 Linux machine `framework.ffr`, which has Nix and Docker and no system Go (`ssh framework.ffr`). Copy a commit there with `git archive HEAD | ssh framework.ffr 'mkdir -p /tmp/kosh-task18 && tar -x -C /tmp/kosh-task18'`, keep everything you make there, caches included, under that one directory, and remove it when you are done. If the sandbox blocks `ssh` or the network, run those commands with the sandbox disabled.

**Interfaces:**
- Consumes: `cmd/kosh` and `cli.Version` (set with `-X github.com/fables-for-robots/kosh/cli.Version=…`); `scripts/test-chart.sh` (Task 16); `scripts/envtest-assets.sh` (Task 8).
- Produces: `nix build` (the program), `nix build .#image` (the image), the development shells `default` and `bare`, the two workflows, `scripts/test-image.sh`. Task 19 adds the end-to-end job to `test.yml` and uses the image.

## Behaviour

**The flake**

1. Inputs as great-machine's: `nixpkgs` at `nixos-26.05`, `systems`, `gonixgo` at `github:draganm/gonixgo/v0.2.0` following both.
2. `packages.default` is kosh built by gonixgo's `buildGoApplication` from `cmd/kosh`, with `CGO_ENABLED = 0`, the ldflag that sets `cli.Version`, and `meta.license = lgpl3Only`. The version is `KOSH_VERSION` from the environment when set (a release, with `--impure`), else the commit's short revision, with `-dirty` for a tree with changes, else `dev`: as great-machine's `version`.
3. The Go that builds it is the one `go.mod` asks for (`go 1.26.x`). Use the `go_1_26` of this nixpkgs if its version is at least `go.mod`'s; if it is older, say so in your report and use what great-machine uses (`go_1_27`), which builds a `go 1.26` module. The Go that resolves the packages while Nix evaluates is the same one (`evalGo`), as in great-machine.
4. The source gonixgo builds from is the repository as it is: kosh has no page to build first, so none of great-machine's `src` and `ui` machinery.
5. `packages.image`, on Linux only: `dockerTools.buildLayeredImage`, name `ghcr.io/fables-for-robots/kosh`, tag `nix`, `includeStorePaths = false`, holding exactly:
   - `/bin/kosh`, copied;
   - `/etc/ssl/certs/ca-certificates.crt`, copied from `cacert`: the relays and number0's DNS are reached over HTTPS;
   - `/etc/passwd` and `/etc/group` with one user, `kosh`, uid and gid 65532, home `/`, shell `/sbin/nologin`;
   - the empty directories `/var/lib/kosh` and `/etc/kosh`, where the chart mounts the state and the key list, and `/tmp` with mode 1777.

   No shell, no busybox, nothing in `/nix/store`. Its config: `Entrypoint = [ "/bin/kosh" ]`, `User = "65532:65532"`, `Env` with `SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt`.
6. `devShells.default` has Go, `kubernetes-helm`, `kubeconform`, `yq-go`, `kind`, `kubectl`, and the built kosh. `devShells.bare` has the same tools without the built program and needs no evaluation option: it is what the workflows and `framework.ffr` use to run `go test`.
7. `.envrc` is great-machine's without what concerns its page: `source_up_if_exists`, `watch_file go.mod go.sum`, `watch_dir` for every top-level directory that holds a tracked Go file, and `use flake . --option allow-unsafe-native-code-during-evaluation true` (read great-machine's last lines for the exact form).

**`scripts/test-image.sh`** takes an image reference, runs with Docker, and fails at the first check that does not hold:

8. `docker run --rm IMAGE version` prints a version.
9. The image runs as uid 65532: `docker inspect` shows the user; a container started with `--read-only` and `id --state-dir /var/lib/kosh` on a `--tmpfs /var/lib/kosh:uid=65532` prints its four lines, which shows that kosh needs no writable root file system.
10. The image has no shell and nothing in `/nix/store`: `docker export` of a created container, listed with `tar -t`, holds no `bin/sh`, no `bin/busybox`, and no entry under `nix/store/`.
11. `docker run --rm IMAGE serve --ssh-listen "" --iroh=false` exits with a status that is not 0 and the message that both transports are off: the entry point is kosh and its errors come out.

**`.github/workflows/test.yml`**, on every push and pull request, as great-machine's in shape (read its steps for how Nix is installed and the evaluation option passed):

12. A job `go`: checkout, Go from `go.mod`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `KUBEBUILDER_ASSETS="$(scripts/envtest-assets.sh)" go test -race ./...`.
13. A job `chart`: `scripts/test-chart.sh`, with Nix providing the tools.
14. A job `nix`: `nix build` with the option; `nix build .#image`; `docker load < result`; `scripts/test-image.sh ghcr.io/fables-for-robots/kosh:nix`.

**`.github/workflows/release.yml`**, on a tag `v*`, as great-machine's:

15. The image for x86_64 and aarch64, each built by Nix on a runner of its architecture with `KOSH_VERSION` set to the tag and `--impure`, checked (`version` prints the tag), pushed as `ghcr.io/fables-for-robots/kosh:<tag>-<arch>`; then a manifest list `ghcr.io/fables-for-robots/kosh:<tag>` over both.
16. The chart: `helm package charts/kosh --version <tag without the v> --app-version <tag>`, and `helm push` of the package to `oci://ghcr.io/fables-for-robots/charts`, after the images are there.
17. Permissions: `contents: read`, `packages: write`.

## Tests

There is no Go to test here. What shows this task works:

- [ ] On this Mac: `nix build --option allow-unsafe-native-code-during-evaluation true` succeeds, and `./result/bin/kosh version` prints a revision. `nix develop .#bare --command go version` prints a Go of at least `go.mod`'s version. Remove `result`.
- [ ] `nix flake check --option allow-unsafe-native-code-during-evaluation true --no-build` evaluates.
- [ ] On `framework.ffr`: `nix build .#image` with the option, `docker load < result`, and `scripts/test-image.sh ghcr.io/fables-for-robots/kosh:nix` passes. See each of its checks fail first by breaking what it checks, once, in a scratch copy (for instance an image with busybox in it for check 10): a check that cannot fail checks nothing. Say in your report what you broke for each and what the script said.
- [ ] On `framework.ffr`, `KOSH_VERSION=v9.9.9 nix build .#image --impure …` gives an image whose `version` prints `v9.9.9`.
- [ ] The workflows are valid YAML and every script and path they name exists (`nix shell nixpkgs#actionlint --command actionlint` passes).
- [ ] Nothing is left behind: no `result` here, the directory on `framework.ffr` removed, the image removed from its Docker.

## Steps

- [ ] `flake.nix`, `flake.lock`, `.envrc`; the program builds here. Commit: `Flake: kosh built by Nix, and the shells to work on it`.
- [ ] The image and `scripts/test-image.sh`, seen to pass and each check seen to fail, on `framework.ffr`. Commit: `Image: kosh, its certificates, and nothing else`.
- [ ] The workflows, linted. Commit: `Workflows: every push is tested, and a tag publishes the image and the chart`.

## Report

What you built; the output of each verification above; which Go the flake uses and why; what you broke to see each check of `scripts/test-image.sh` fail; anything in great-machine's files that you did not carry over and why.
