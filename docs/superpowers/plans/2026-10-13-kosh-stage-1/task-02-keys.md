# Task 2: keys

The key list: the file that says whose SSH key kosh accepts, and whether as a full key or a read-only one. This package decides who gets in, so it fails closed everywhere.

**Read first:** the spec, §2 and §6 (`docs/superpowers/specs/2026-10-13-kosh-stage-1-design.md`); the plan's Global Constraints and Review Focus (`docs/superpowers/plans/2026-10-13-kosh-stage-1.md`); `keys/keys.go`, which is the contract.

**Files:**
- Modify: `keys/keys.go` (the bodies behind the signatures that are there; the signatures and their documentation stay)
- Create: `keys/keys_test.go`, `keys/watch_test.go`

**Interfaces:**
- Consumes: `golang.org/x/crypto/ssh` (`ParseAuthorizedKey`, `FingerprintSHA256`).
- Produces, exactly as `keys/keys.go` declares them: `Grant`, `Rejected`, `Set`, `Parse`, `(*Set).Lookup`, `(*Set).Len`, `List`, `NewList`, `Open`, `(*List).Watch`, `(*List).Lookup`, `(*List).Replace`, `(*List).OnChange`, `OptionReadOnly`.

## Behaviour

**Parse**

1. A line is `[options] keytype base64 [comment]`, as `ssh.ParseAuthorizedKey` reads one. Parse the file line by line yourself and give each line to it, so that line numbers are yours and one bad line cannot swallow the next.
2. Empty lines, lines of white space only, and lines whose first character that is not white space is `#` are skipped. They are not rejected.
3. No option: a full key. The single option `read-only`, in any case (`Read-Only`): a read-only key.
4. Any other option, alone or beside `read-only` (`readonly`, `no-pty`, `command="x"`, `read-only,no-pty`): the line is rejected, with a reason that names the option. A line that `ParseAuthorizedKey` cannot read is rejected with its error as the reason. A rejected line contributes no key.
5. `Rejected.Line` counts every line of the file from 1, skipped ones included.
6. `Grant.Name` is the comment, whole, with the spaces it has inside (`Dragan's laptop`); when the comment is empty it is the fingerprint. `Grant.Fingerprint` is `ssh.FingerprintSHA256(key)`.
7. A key on several accepted lines: read-only if any of them says so, the name of the first.
8. What people write (Review Focus 5): a UTF-8 byte order mark at the start of the file is ignored; `\r\n` line ends work; a last line without a line feed works; a line longer than 64 KiB is rejected with a reason that says it is too long, and the lines after it are still read. `bufio.Scanner` with its default buffer fails the whole read on such a line: do not let it.
9. Every key type `ssh.ParseAuthorizedKey` accepts is accepted, except a certificate (`*ssh.Certificate`), which is rejected with the reason `certificates are not accepted: list the key itself`. (Added after the task's review.)
9a. A line whose comment begins with the word `read-only`, in any case, is rejected with the reason `read-only is an option and goes in front of the key`. (Added after the task's review.)
10. `Parse` never fails. A reader that returns an error mid-way yields the keys read so far and one `Rejected` whose reason is the error, with the number of the line it happened on.

**Set**

11. A nil `*Set` and the zero `Set` hold no key: `Lookup` returns false, `Len` returns 0. Neither panics.
12. `Lookup` compares keys by their wire format (`key.Marshal()`).

**List**

13. `NewList(nil)` is a list that holds no key.
14. `Replace` puts the set in place and then calls every `OnChange` function, in the order they were registered, in the caller's goroutine. A function that calls `Lookup` sees the new set. `Replace` must not hold the list's lock while it calls them: a function may call `Lookup` and `OnChange`.
15. All methods of `List` may be called from any goroutine at once.

**Open and Watch**

16. `Open`: a file that cannot be read is an error that names the path. Each rejected line is logged at warn level with its line number and reason. A file with no accepted key is logged at warn level as holding no key, and is not an error. One info line says how many keys the list holds.
17. `Watch` reads the file every `interval`. It compares the content with what it read last (the bytes, or a hash of them, not the modification time) and calls `Replace` only when it differs.
18. A file that cannot be read during `Watch` is an empty set: `Replace` with a set of no keys, once, with an error in the log. When it can be read again its keys are back.
19. A changed file is logged as in 16, with the counts.
20. A ConfigMap in a pod changes by its directory's symbolic link being swapped; reading the path again by name each time is what makes that work. Do not hold the file open and do not watch an inode.
21. `Watch` returns when its context is done. A `List` that `NewList` made has no path: `Watch` on it returns at once.

## Tests

Generate test keys in the tests (`ed25519.GenerateKey`, `ssh.NewPublicKey`, `ssh.MarshalAuthorizedKey`); write no key into the repository. Capture the log with a `slog.Handler` of the test's own.

`keys/keys_test.go`:

| Test | What it shows |
|---|---|
| `TestParseFullAndReadOnly` | one plain line, one `read-only` line: two grants, `ReadOnly` false and true, names from the comments |
| `TestParseSkipsCommentsAndBlankLines` | `#` lines, blank lines, a line of spaces: no key, nothing rejected |
| `TestParseRejectsUnknownOptions` | table: `readonly`, `no-pty`, `command="echo hi"`, `read-only,no-pty`, `restrict` each rejected, reason contains the option, the key is not in the set |
| `TestParseReadOnlyInAnyCase` | `Read-Only` and `READ-ONLY` are read-only |
| `TestParseRejectsGarbage` | `not a key`, a truncated base64 key: rejected, the good line after them accepted |
| `TestParseLineNumbers` | a file with comments and blanks before a bad line: `Rejected.Line` is the line's number in the file |
| `TestParseNameFallsBackToFingerprint` | no comment: `Name == Fingerprint`, which starts with `SHA256:` |
| `TestParseCommentWithSpaces` | the whole comment is the name |
| `TestParseDuplicateKey` | full then read-only, and read-only then full: read-only both times; the name is the first line's |
| `TestParseAsPeopleWriteThem` | BOM, CRLF, no final line feed: all keys accepted; a 100 kB line: rejected as too long, the next line accepted |
| `TestParseKeyTypes` | ed25519, ecdsa P-256 and rsa 2048 keys are accepted |
| `TestParseReaderError` | a reader that fails after two lines: the first two keys, one rejection with the error |
| `TestNilSetHoldsNothing` | nil and zero `Set`: `Lookup` false, `Len` 0 |
| `TestListReplaceCallsOnChangeAfterTheSetIsInPlace` | the function looks the new key up and finds it; two functions are called in order |
| `TestListOnChangeMayUseTheList` | a function that calls `Lookup` and `OnChange` does not deadlock |
| `TestListConcurrentUse` | `Lookup`, `Replace` and `OnChange` from many goroutines under `-race` |

`keys/watch_test.go` (intervals of 5 ms; wait for a condition with a deadline of 2 s, never a fixed sleep):

| Test | What it shows |
|---|---|
| `TestOpenMissingFile` | an error that contains the path |
| `TestOpenLogsRejectedLinesAndCount` | warn lines with line numbers, an info line with the count |
| `TestOpenEmptyFileWarns` | no error, a warning |
| `TestWatchTakesOverAChangedFile` | a key added to the file is found; `OnChange` was called once |
| `TestWatchIgnoresAnUnchangedFile` | the file rewritten with the same content: `OnChange` not called |
| `TestWatchUnreadableFileIsEmpty` | the file removed: the key is no longer found, `OnChange` called once; the file written again: the key is back |
| `TestWatchFollowsASwappedSymlink` | `dir/..data -> v1`, the path is `dir/..data/authorized_keys`; `..data` is re-pointed to `v2` with a rename: the new key is found |
| `TestWatchEndsWithItsContext` | cancel: `Watch` returns |

## Steps

- [ ] Write `keys/keys_test.go`. Run `go test ./keys/` and see every test fail on `not implemented`.
- [ ] Implement `Parse`, `Set`, `List` in `keys/keys.go`. Run until `go test -race ./keys/ -run 'TestParse|TestNil|TestList'` passes.
- [ ] Commit: `Keys: the key list is parsed, and what is not understood gives no access`.
- [ ] Write `keys/watch_test.go`; see it fail.
- [ ] Implement `Open` and `Watch`. Run until `go test -race ./keys/` passes.
- [ ] `gofmt -l . && go vet ./... && go build ./...` are clean.
- [ ] Commit: `Keys: the list follows its file, and a file that cannot be read is an empty list`.

## Report

What you built and the test results; anything in the contract that did not fit; any behaviour above you could not test and why.
