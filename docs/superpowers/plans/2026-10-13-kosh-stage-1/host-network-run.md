# kosh on the host's network, run once

Taken from the report of the task that ran it (Fix I of stage 1's execution, 2026-10-13, on a kind cluster on an x86_64 Linux machine).

The addresses of the test machine's own networks are replaced here by documentation addresses (192.0.2.x for its LAN, 198.51.100.x for its overlay networks, 203.0.113.7 for its public address); the addresses of the kind cluster are as they were.

## 1. kosh on the host's network, run

On `framework.ffr`: kind cluster `kosh-fixi-b8d57fdf` with its own kubeconfig;
the image built by Nix from the merged tree (`a78fe7c`), tagged
`kosh-fixi:b8d57fdf`; a state Secret and a key list as `scripts/test-e2e.sh`
makes them; the chart with `iroh.hostNetwork=true`, `iroh.port=45111`.

### What kosh announces there

kosh's log: `mDNS announces … addrs="[172.18.0.2:45111 10.244.0.1:45111]"`.

The pkarr record (`dig +short @1.1.1.1 TXT _iroh.oya6owds….dns.iroh.link`):

```
"relay=https://euc1-1.relay.n0.iroh-canary.iroh.link./"
"addr=10.244.0.1:45111"
"addr=203.0.113.7:45111"
"addr=172.18.0.2:45111"
```

The node's interfaces (`ip -br addr` in the node), and each announced address
from the box and from a container on the node's network:

| Interface | Addresses | Announced | Reachable |
|---|---|---|---|
| `lo` | `127.0.0.1` | no | |
| `eth0` | `172.18.0.2/16`, an IPv6 ULA, link-local | `172.18.0.2` (the socket is bound to `0.0.0.0`: IPv4 only) | **yes** (ping from the box and from the container) |
| `veth1d67d65a`, `veth49d1988c`, `veth982e0708` (kindnet's, one per pod) | each `10.244.0.1/32`, link-local | `10.244.0.1`, once | **no**: from the box it is routed to the LAN's gateway (`ip route get`: `via 192.0.2.1 dev wlp166s0`), no answer to ping; from a container on the node's network none either |
| (none) | `203.0.113.7` | in the pkarr record only: the public address as the relay sees it | not tried (the box's own NAT) |

So the plausible failure of the brief is real as far as the announcement goes:
the pods' gateway address, which nobody outside the node reaches, is in the
mDNS record and in the pkarr record. With the pod network (the control) the
record holds `addr=10.244.0.5:46582` (the pod's address, unreachable from
outside too) and `addr=203.0.113.7:46582`.

### Connections

Each connection is `iroh-ssh -i key -o StrictHostKeyChecking=no -o
UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes kosh@<id> true`, one after
the other, 2 seconds apart; "reached" means it printed `kosh runs no commands`
(kosh's SSH server, after authentication), which is not the UI on a terminal
but is every step up to it; the time is the whole command's. The `iroh-ssh`
built from the repository is `82e9971` (iroh 1.3.0); the released one is 0.2.12
(SHA-256 as `test-e2e.sh` knows it).

| # | kosh | Client, and where | Reached of 20 | Seconds: min / median / max | Found through |
|---|---|---|---|---|---|
| 1 | host network, relay on, first image | git `iroh-ssh`, on the box | 20 | 0.12 / 0.22 / 0.29 | the pkarr record (see below), direct to `172.18.0.2` |
| 2 | host network, relay on, first image | released 0.2.12, on the box | 20 | 0.06 / 0.07 / 0.14 | the pkarr record, direct |
| 3 | host network, **relay off**, first image | git, on the box | 20 | 0.11 / 0.16 / 0.23 | the pkarr record of the installation before, still in DNS (see below); not mDNS |
| 4 | host network, relay off, first image | git, **in a container on the node's network** | 20 | 1.13 / 1.16 / 1.50 | mDNS alone |
| 5 | host network, relay off, **image with the announcer's change** | git, in a container on the node's network | 20 | 1.13 / 1.16 / 1.24 | mDNS alone |
| 6 | control: pod network, mDNS off | git, on the box | 20 | 1.36 / 1.70 / 2.88 | the pkarr record, the relay |
| 7 | control: pod network, mDNS off | released, on the box | 20 | 1.41 / 1.78 / 2.13 | the pkarr record, the relay |

No connection of these 140 failed, and none took 5 seconds. With the
unreachable `10.244.0.1` in the announcement and in the record, a direct
connection is made in 0.1 to 0.3 s through the record and in 1.2 s through mDNS
(of which about a second is the lookup: the same 1.1 s per connection as in
Fix C's runs, where every announced address was reachable). **The condition of
point 3 is not met, and nothing of it is built.** The relay path is not hurt by
the record's addresses either (rows 6 and 7 against 1 and 2: the host network
is faster, because a direct address in the record works).

**What the box could not show, and how rows 4 and 5 came about.** The brief
expected the box to reach the node by mDNS. It does not: `iroh-ssh`'s mDNS
client sends its questions on one interface, the one of the default route
(seen with a listener on the box: its questions, 76 and 89 bytes, leave from
`192.0.2.155` on `wlp166s0` and never on the kind bridge), and the node is
not on that link. Rows 1 and 3 looked like mDNS and were not: row 3 passed
with the relays off because the record of row 1's installation was still in
DNS with the same address and port; once the control had published its own
record (another port), the same client against the same kosh timed out
(`Error: timed out`, 30 s), with the first image as with the second. That is
inference from those two observations, not a trace of the client. For mDNS
proper I ran the same git `iroh-ssh` in a container on the node's network
(`alpine:latest`, which was on the box, with `/nix/store` mounted read-only for
the binary and `ssh`; container `kosh-fixi-client-b8d57fdf`, removed
afterwards), where the node is on the default link: rows 4 and 5. A listener on
the box's bridge also got kosh's answer to a browse (`172.18.0.2:5353`, 266
bytes), with the second image.

**Two connections outside the table.**

- In a first run without the 2 seconds between connections, the 28th
  connection in 15 seconds (released client, row 2's setting) hung for 60 s and
  failed: kosh's log has `connection closed after authenticating … reason="too
  many connections of this key are open"`. It is the limit of 16 connections
  per key in `sshd`, reached because connections whose session has ended stay
  open on the server for a while (next paragraph), not the network. I added
  the pause and ran rows 1 and 2 again. Two things in it are worth a look by
  whoever owns `sshd`: a fast series of connections of one key is refused for
  a reason the user never sees, and the client did not learn of the refusal:
  it hung until its own timeout.
- The row "host network, relay off, second image, git on the box" exists (19
  timed out, 1 reached in 6 s) and means nothing: it is the client that cannot
  reach the node by mDNS, and I reinstalled kosh under it while finding that
  out.

**The `timeout: no recent network activity` endings (the dispatch's item 4), in
passing.** What I have seen fits a client that goes away without closing, not
kosh's two seconds of grace. In Fix C's probe logs the connections that ended
that way had ended their session with status 0 and then never delivered SSH's
own disconnect message (`disconnected by user`, which the others did): the
last thing `ssh` wrote into its `ProxyCommand` did not arrive. `iroh-ssh proxy`
ends when `ssh` closes its pipe, and evidently sometimes before its QUIC stack
has sent what was written last and the close. The server is then still
reading, so kosh's `Close` and its grace have not begun; it waits for the
QUIC idle timeout. This round adds that such connections count against the 16
per key while they wait, and that the released client leaves more of them than
the one on iroh 1.x (the limit was reached after 27 connections in 15 s). I did
not trace a client to confirm it.


## 3. Not built

See 1. What I would change if the record's addresses ever hurt: not the
record's shape, but the address rule (an address that is a `/32` on a
broadcast interface, or that several interfaces share, is not an address of a
LAN), and the chart's value to publish the relay alone as the brief suggests.
Nothing measured asks for either.


## What I could not find out

- A client on another machine of the node's LAN: the container on the node's
  Docker network stands for it.
- Whether an unreachable address slows a client that hears *only* it (a pod,
  through the one veth still joined): not tried; a pod reaches `10.244.0.1`
  anyway.
- The announcer's leave, its goodbye on the left interface, and the kernel's
  real refusal at 20 memberships, on a real machine: only with fakes.
- Why the released client leaves so many connections open on the server
  (item 4): described, not traced.

