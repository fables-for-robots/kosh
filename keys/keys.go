// Package keys is the list of accepted keys: the file in OpenSSH's
// authorized_keys format that says whose SSH key kosh accepts, and
// whether as a full key or a read-only one.
//
// A line is a key, with the option read-only in front or none, and a
// comment after it that is the key's name. A line with any other option,
// and a line that does not parse, is rejected: it gives no access. Empty
// lines and lines that begin with # are skipped.
package keys

import (
	"context"
	"io"
	"log/slog"
	"time"

	"golang.org/x/crypto/ssh"
)

// OptionReadOnly is the one option of a line that kosh knows.
const OptionReadOnly = "read-only"

// Grant is what the key list says about one key.
type Grant struct {
	// Name is the key's comment, or its fingerprint when it has none.
	Name string
	// Fingerprint is the SHA256 fingerprint of the key, as ssh-keygen -l
	// prints it: "SHA256:…".
	Fingerprint string
	// ReadOnly is true for a key that may change nothing and read no
	// Secret's contents.
	ReadOnly bool
}

// Rejected is a line of the key list that gives no access, and why.
type Rejected struct {
	Line   int // counted from 1
	Reason string
}

// Set is one reading of the key list. The zero value and nil hold no key.
type Set struct {
	grants map[string]Grant // by the key's wire format
}

// Parse reads a key list. It does not fail: a line it cannot accept is in
// the rejected ones, and the others are in the set. A key that is on more
// than one line is read-only if any of its lines says so, and has the
// name of its first line.
func Parse(r io.Reader) (*Set, []Rejected) {
	panic("not implemented")
}

// Lookup returns the grant of key.
func (s *Set) Lookup(key ssh.PublicKey) (Grant, bool) {
	panic("not implemented")
}

// Len returns the number of keys.
func (s *Set) Len() int {
	panic("not implemented")
}

// List is a key list that changes: the set that is current, and those
// who are told when another takes its place. All methods may be called
// from any goroutine.
type List struct {
	_ noCopy
}

type noCopy struct{}

// NewList returns a list that holds set until Replace gives it another.
// It is what tests use, and what Open is made of.
func NewList(set *Set) *List {
	panic("not implemented")
}

// Open reads the file at path and returns its list. A file that cannot
// be read is an error; rejected lines are logged and are none. A file
// without a key is accepted, with a warning in the log.
func Open(path string, log *slog.Logger) (*List, error) {
	panic("not implemented")
}

// Watch reads the file Open was given again every interval until ctx is
// done, and replaces the set when the file's content has changed. A file
// that cannot be read then is an empty set, until it can be read again.
// Watch returns when ctx is done. It is for a list that Open returned.
func (l *List) Watch(ctx context.Context, interval time.Duration) {
	panic("not implemented")
}

// Lookup returns the grant of key in the current set.
func (l *List) Lookup(key ssh.PublicKey) (Grant, bool) {
	panic("not implemented")
}

// Replace makes set the current one and calls every function OnChange
// was given, in the goroutine of the caller, after the set is in place.
func (l *List) Replace(set *Set) {
	panic("not implemented")
}

// OnChange registers fn to be called after every Replace.
func (l *List) OnChange(fn func()) {
	panic("not implemented")
}
