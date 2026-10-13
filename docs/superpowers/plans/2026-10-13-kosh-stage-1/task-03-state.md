# Task 3: state

What kosh keeps between starts: its iroh key, whose public half is the endpoint ID that users dial, and its SSH host key. Two files in one directory. A key that is silently replaced changes kosh's identity for every user, so nothing here ever writes over a file that exists.

**Read first:** the spec, §2 and §7 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); `state/state.go`, which is the contract. For the iroh key file the sibling projects' reader and writer are the model: `~/go/pkg/mod/github.com/amber-store/jaccard-store@v0.7.0/node/key.go` (read it; do not import it).

**Files:**
- Modify: `state/state.go` (the bodies; signatures and documentation stay)
- Create: `state/state_test.go`

**Interfaces:**
- Consumes: `github.com/tmc/go-iroh/key` (`SecretKey`, `GenerateSecretKey`, `NewSecretKey`, `SeedSize`, `(SecretKey).Bytes`, `(SecretKey).Public`), `golang.org/x/crypto/ssh` (`MarshalPrivateKey`, `ParsePrivateKey`, `NewSignerFromKey`, `MarshalAuthorizedKey`, `FingerprintSHA256`), `crypto/ed25519`, `encoding/pem`.
- Produces, exactly as `state/state.go` declares them: `IrohKeyFile`, `HostKeyFile`, `State`, `Load`, `(State).EndpointID`, `(State).HostKeyLine`, `(State).HostKeyFingerprint`.

## Behaviour

1. `Load(dir)` reads `dir/iroh.key` and `dir/ssh_host_ed25519_key`. It creates `dir` with mode 0700 when it is missing, and each file that is missing with a new key and mode 0600.
2. **`iroh.key`** is 64 hex characters and a line feed. Hex in either case and white space around it are read. Anything else is an error: more than 1024 bytes, another length, not hex, a seed of all zeros (which is what a placeholder holds).
3. **`ssh_host_ed25519_key`** is created as a new ed25519 key in OpenSSH's private key format (`ssh.MarshalPrivateKey(key, "")`, PEM-encoded). When it is read, any private key `ssh.ParsePrivateKey` accepts is taken, of whatever type: the operator may have put their own there. A key with a passphrase is an error.
4. **A file that is there and does not parse is an error and is left exactly as it is.** The error names the path and what is wrong, and repeats none of the file's contents: they may be most of a key.
5. **A file is created whole or not at all.** Write it under a temporary name in the same directory, sync and close it, then give it its name with `os.Link`, which fails when the name exists; remove the temporary name. When the link fails with `fs.ErrExist`, another `Load` got there first: read what it wrote. Several `Load` of one empty directory at once all return the same keys.
6. A directory that cannot be written to is fine when both files are there, and an error that names the missing file's path when one is not.
7. One file there and the other missing: the one is kept, the other created.
8. `EndpointID()` is the public half of the iroh key as 64 lower-case hex characters. Look at how `key.PublicKey` prints itself and make sure this is hex, not base32.
9. `HostKeyLine()` is `ssh.MarshalAuthorizedKey` of the host key's public half without its trailing line feed: `ssh-ed25519 AAAA…`.
10. `HostKeyFingerprint()` is `ssh.FingerprintSHA256` of it.
11. `State.HostKeyPEM` is the file's bytes as read; `State.HostKey` signs with that key.

## Tests

`state/state_test.go`, each in `t.TempDir()`:

| Test | What it shows |
|---|---|
| `TestLoadCreatesBothKeys` | an empty directory: both files exist with mode 0600; `iroh.key` is 64 hex characters and `\n`; the host key file begins with `-----BEGIN OPENSSH PRIVATE KEY-----` |
| `TestLoadCreatesTheDirectory` | a path that does not exist is made with mode 0700 |
| `TestLoadIsStable` | a second `Load` returns the same `EndpointID`, `HostKeyLine` and `HostKeyFingerprint` |
| `TestEndpointIDIsHex` | 64 characters of `0-9a-f` |
| `TestHostKeyLineAndFingerprint` | the line starts with `ssh-ed25519 ` and has no line feed and no comment; the fingerprint starts with `SHA256:`; `ssh.ParseAuthorizedKey` of the line gives a key whose fingerprint is that one |
| `TestHostKeySigns` | a signature of `State.HostKey` verifies with the public key of `HostKeyLine` |
| `TestIrohKeyFileForms` | upper-case hex, and hex with spaces and line feeds around it, are read as the same key |
| `TestIrohKeyFileThatDoesNotParse` | table: 63 characters, 64 characters that are not hex, 64 zeros, 2000 bytes. Each: an error that contains the path and does not contain the file's content; the file's bytes are unchanged afterwards |
| `TestHostKeyFileThatDoesNotParse` | `garbage` in the file: an error with the path; the file is unchanged |
| `TestHostKeyOfAnotherType` | an RSA key written in OpenSSH format is accepted, and `HostKeyLine` starts with `ssh-rsa ` |
| `TestOneFileMissing` | remove one of the two after a first `Load`: the other's key is the same after the second `Load`, and the removed one is new |
| `TestReadOnlyDirectory` | files there, directory mode 0500: `Load` works. One file removed first: an error that names that file's path. Skip when running as root (`os.Geteuid() == 0`). Restore the mode in `t.Cleanup` so the directory can be removed |
| `TestConcurrentLoad` | eight goroutines `Load` one empty directory: all succeed with one `EndpointID` and one `HostKeyLine`; no temporary file is left in the directory |

## Steps

- [ ] Write `state/state_test.go`. Run `go test ./state/` and see the tests fail on `not implemented`.
- [ ] Implement `state/state.go`. Run until `go test -race ./state/` passes.
- [ ] `gofmt -l . && go vet ./... && go build ./...` are clean.
- [ ] Commit: `State: the iroh key and the host key are read from a directory, made when missing, and never written over`.

## Report

What you built and the test results; anything in the contract that did not fit; how `key.PublicKey` prints and what you did for `EndpointID`.
