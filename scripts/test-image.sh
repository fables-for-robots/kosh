#!/usr/bin/env bash
# Tests the image that `nix build .#image` makes, once it is loaded into
# Docker: that it is kosh, runs as kosh's user on a read-only root, and
# holds no shell and nothing in /nix/store. Stops at the first check that
# does not hold. With VERSION, kosh version has to print exactly that.
#
#   nix build .#image --option allow-unsafe-native-code-during-evaluation true
#   docker load < result
#   scripts/test-image.sh ghcr.io/fables-for-robots/kosh:nix [VERSION]
set -euo pipefail

if [ $# -lt 1 ] || [ $# -gt 2 ]; then
  echo "usage: $0 IMAGE [VERSION]" >&2
  exit 2
fi
IMAGE=$1
WANT_VERSION=${2:-}

OUT=$(mktemp -d)
CONTAINER=
cleanup() {
  [ -z "$CONTAINER" ] || docker rm "$CONTAINER" >/dev/null || true
  rm -rf "$OUT"
}
trap cleanup EXIT

fail() { echo "FAIL: $*" >&2; exit 1; }

# 8. The entry point is kosh, and it says what it is.
echo "== version"
docker run --rm "$IMAGE" version >"$OUT/version" 2>"$OUT/version.err" ||
  { cat "$OUT/version.err" >&2; fail "version: kosh version failed"; }
[ "$(grep -c . "$OUT/version")" = 1 ] ||
  fail "version: want one line, got: $(cat "$OUT/version")"
if [ -n "$WANT_VERSION" ] && [ "$(cat "$OUT/version")" != "$WANT_VERSION" ]; then
  fail "version: kosh version printed '$(cat "$OUT/version")', want '$WANT_VERSION'"
fi
echo "ok   version $(cat "$OUT/version")"

# 9. The user the image runs as is 65532, the one the chart's
# securityContext names. Without capabilities, a process of any other
# user, root included, cannot write to a directory of mode 0700 owned by
# 65532: kosh making its keys there on a read-only root shows both that
# it runs as 65532 and that it needs nothing writable but its state.
echo "== user"
user=$(docker inspect --format '{{.Config.User}}' "$IMAGE") || fail "user: docker inspect $IMAGE failed"
[ "$user" = "65532:65532" ] || fail "user: the image's user is '$user', want 65532:65532"
echo "ok   the image's user is $user"
docker run --rm --read-only --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /var/lib/kosh:uid=65532,gid=65532,mode=0700 \
  "$IMAGE" id --state-dir /var/lib/kosh >"$OUT/id" 2>"$OUT/id.err" ||
  { cat "$OUT/id.err" >&2; fail "user: kosh id on a read-only root failed"; }
[ "$(grep -c . "$OUT/id")" = 4 ] || fail "user: kosh id printed, want four lines: $(cat "$OUT/id")"
for field in "endpoint ID" "iroh-ssh" "host key" "fingerprint"; do
  grep -q "^$field " "$OUT/id" || fail "user: kosh id printed no line '$field': $(cat "$OUT/id")"
done
echo "ok   kosh id as 65532 on a read-only root"

# 10. kosh and nothing else: no shell, no busybox, and nothing in
# /nix/store, as the file system of a container made from the image
# holds it.
echo "== contents"
CONTAINER=$(docker create "$IMAGE")
docker export "$CONTAINER" | tar -t | sed 's|^\./||' >"$OUT/files"
for f in bin/sh bin/busybox; do
  ! grep -qx "$f" "$OUT/files" || fail "contents: the image has $f"
done
echo "ok   no bin/sh, no bin/busybox"
store=$(grep '^nix/store/.' "$OUT/files" || true)
[ -z "$store" ] || fail "contents: the image has, in nix/store: $(echo "$store" | head -5)"
echo "ok   nothing in nix/store"

# 11. kosh's errors come out: with both transports off it refuses to
# start, says why, and exits with a status that is not 0.
echo "== refusal"
if docker run --rm "$IMAGE" serve --ssh-listen "" --iroh=false >"$OUT/serve" 2>"$OUT/serve.err"; then
  fail "refusal: kosh serve with both transports off exited 0"
fi
grep -qF "both transports are off" "$OUT/serve.err" ||
  { cat "$OUT/serve" "$OUT/serve.err" >&2; fail "refusal: kosh serve did not say that both transports are off"; }
echo "ok   refused: $(head -1 "$OUT/serve.err")"

echo "all checks passed"
