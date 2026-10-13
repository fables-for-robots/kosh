#!/usr/bin/env bash
# Tests the image that `nix build .#image` makes, once it is loaded into
# Docker: that it is kosh, runs as kosh's user on a read-only root, and
# holds kosh, its certificates, its user and the licence texts, and
# nothing else. Stops at the first check that
# does not hold. With VERSION, kosh version has to print exactly that.
#
#   nix build .#image --option allow-unsafe-native-code-during-evaluation true
#   docker load < result
#   scripts/test-image.sh ghcr.io/fables-for-robots/kosh:nix [VERSION]
#
# Docker runs only an image it has: the script never pulls one.
set -euo pipefail
# sort and comm have to agree on the order.
export LC_ALL=C

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
docker run --pull never --rm "$IMAGE" version >"$OUT/version" 2>"$OUT/version.err" ||
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
docker run --pull never --rm --read-only --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /var/lib/kosh:uid=65532,gid=65532,mode=0700 \
  "$IMAGE" id --state-dir /var/lib/kosh >"$OUT/id" 2>"$OUT/id.err" ||
  { cat "$OUT/id.err" >&2; fail "user: kosh id on a read-only root failed"; }
[ "$(grep -c . "$OUT/id")" = 4 ] || fail "user: kosh id printed, want four lines: $(cat "$OUT/id")"
for field in "endpoint ID" "iroh-ssh" "host key" "fingerprint"; do
  grep -q "^$field " "$OUT/id" || fail "user: kosh id printed no line '$field': $(cat "$OUT/id")"
done
echo "ok   kosh id as 65532 on a read-only root"

# 10. kosh and nothing else: the file system of a container made from
# the image holds exactly the files and directories below, of which
# bin/kosh is the one program, and the certificates are there.
echo "== contents"
# What the image holds: kosh, the certificates, its user, the licence
# texts, and the directories the flake makes.
image_files=(
  bin bin/kosh
  etc etc/group etc/passwd etc/kosh etc/ssl etc/ssl/certs etc/ssl/certs/ca-certificates.crt
  licenses licenses/LICENSE licenses/COPYING licenses/NOTICE licenses/Apache-2.0.txt
  tmp var var/lib var/lib/kosh
)
# What Docker adds to the file system of every container, whatever its
# image: these do not come from the image.
docker_files=(
  .dockerenv dev dev/console dev/pts dev/shm proc sys
  etc/hostname etc/hosts etc/mtab etc/resolv.conf
)
CONTAINER=$(docker create --pull never "$IMAGE")
docker export "$CONTAINER" >"$OUT/export.tar"
# Each name once, without a leading ./ or a trailing /.
tar -tf "$OUT/export.tar" | sed -e 's|^\./||' -e 's|/$||' | grep . | sort -u >"$OUT/files"
printf '%s\n' "${image_files[@]}" "${docker_files[@]}" | sort -u >"$OUT/want"
extra=$(comm -23 "$OUT/files" "$OUT/want" | tr '\n' ' ')
[ -z "$extra" ] || fail "contents: the image has what it should not: $extra"
missing=$(comm -13 "$OUT/files" "$OUT/want" | tr '\n' ' ')
[ -z "$missing" ] || fail "contents: the image lacks: $missing"
echo "ok   exactly kosh, its certificates, its user, the licences and its directories"
# Docker's files are empty placeholders with the execute bit; of the
# image's own, only bin/kosh is a program.
printf '%s\n' "${docker_files[@]}" >"$OUT/docker"
programs=$(tar -tvf "$OUT/export.tar" | awk '$1 ~ /^-/ && $1 ~ /x/ { print $NF }' |
  sed 's|^\./||' | grep -vxF -f "$OUT/docker" | tr '\n' ' ' || true)
[ "$programs" = "bin/kosh " ] || fail "contents: the executable files are '$programs', want only bin/kosh"
echo "ok   bin/kosh is the only program"
tar -xOf "$OUT/export.tar" etc/ssl/certs/ca-certificates.crt >"$OUT/ca.crt" ||
  fail "contents: etc/ssl/certs/ca-certificates.crt cannot be read"
grep -q "BEGIN CERTIFICATE" "$OUT/ca.crt" || fail "contents: etc/ssl/certs/ca-certificates.crt holds no certificate"
echo "ok   the CA bundle holds $(grep -c "BEGIN CERTIFICATE" "$OUT/ca.crt") certificates"

# 11. kosh's errors come out: with both transports off it refuses to
# start, says why, and exits with a status that is not 0.
echo "== refusal"
if docker run --pull never --rm "$IMAGE" serve --ssh-listen "" --iroh=false >"$OUT/serve" 2>"$OUT/serve.err"; then
  fail "refusal: kosh serve with both transports off exited 0"
fi
grep -qF "both transports are off" "$OUT/serve.err" ||
  { cat "$OUT/serve" "$OUT/serve.err" >&2; fail "refusal: kosh serve did not say that both transports are off"; }
echo "ok   refused: $(head -1 "$OUT/serve.err")"

echo "all checks passed"
