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
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
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
	set := &Set{grants: map[string]Grant{}}
	var rejected []Rejected
	br := bufio.NewReader(r)
	for n := 1; ; n++ {
		line, long, err := readLine(br)
		if err != nil && !errors.Is(err, io.EOF) {
			// What was read of this line is not used: a line cut short
			// could still parse, as something its author did not write.
			rejected = append(rejected, Rejected{Line: n, Reason: "reading the key list: " + err.Error()})
			break
		}
		if err != nil && len(line) == 0 && !long {
			break // the end, after a line feed or in an empty file
		}
		if n == 1 {
			line = bytes.TrimPrefix(line, byteOrderMark)
		}
		if long {
			rejected = append(rejected, Rejected{Line: n, Reason: "the line is too long"})
		} else if reason := set.add(line); reason != "" {
			rejected = append(rejected, Rejected{Line: n, Reason: reason})
		}
		if err != nil {
			break
		}
	}
	return set, rejected
}

// maxLine is the length of the longest line Parse reads, without its line
// end. The longest key OpenSSH writes, an RSA key of 16384 bits, is under
// 3 KiB.
const maxLine = 64 << 10

var byteOrderMark = []byte("\xef\xbb\xbf")

// readLine returns the next line of br without its line end, "\n" or
// "\r\n". A line longer than maxLine is read to its end and not kept: long
// is true. err is io.EOF with the last line when it has no line feed, and
// with nothing after a line feed.
func readLine(br *bufio.Reader) (line []byte, long bool, err error) {
	for {
		var frag []byte
		frag, err = br.ReadSlice('\n')
		if !long {
			if len(line)+len(frag) > maxLine+len("\r\n") {
				long, line = true, nil
			} else {
				line = append(line, frag...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if !long {
			line = bytes.TrimSuffix(bytes.TrimSuffix(line, []byte("\n")), []byte("\r"))
			if len(line) > maxLine {
				long, line = true, nil
			}
		}
		return line, long, err
	}
}

// add puts the key of line into s. It returns why the line gives no
// access, or "" when it does or is a line to skip.
func (s *Set) add(line []byte) (reason string) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 || trimmed[0] == '#' {
		return ""
	}
	key, comment, options, _, err := ssh.ParseAuthorizedKey(line)
	if err != nil {
		return err.Error()
	}
	readOnly := false
	var unknown []string
	for _, o := range options {
		if strings.EqualFold(o, OptionReadOnly) {
			readOnly = true
		} else {
			unknown = append(unknown, o)
		}
	}
	if len(unknown) > 0 {
		return "unknown option " + strings.Join(unknown, ", ")
	}
	wire := string(key.Marshal())
	if g, ok := s.grants[wire]; ok {
		g.ReadOnly = g.ReadOnly || readOnly
		s.grants[wire] = g
		return ""
	}
	fp := ssh.FingerprintSHA256(key)
	name := comment
	if name == "" {
		name = fp
	}
	s.grants[wire] = Grant{Name: name, Fingerprint: fp, ReadOnly: readOnly}
	return ""
}

// Lookup returns the grant of key.
func (s *Set) Lookup(key ssh.PublicKey) (Grant, bool) {
	if s == nil || key == nil {
		return Grant{}, false
	}
	g, ok := s.grants[string(key.Marshal())]
	return g, ok
}

// Len returns the number of keys.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.grants)
}

// List is a key list that changes: the set that is current, and those
// who are told when another takes its place. All methods may be called
// from any goroutine.
type List struct {
	_ noCopy

	mu       sync.Mutex
	set      *Set
	onChange []func()
}

type noCopy struct{}

// NewList returns a list that holds set until Replace gives it another.
// It is what tests use, and what Open is made of.
func NewList(set *Set) *List {
	return &List{set: set}
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
	l.mu.Lock()
	set := l.set
	l.mu.Unlock()
	return set.Lookup(key)
}

// Replace makes set the current one and calls every function OnChange
// was given, in the goroutine of the caller, after the set is in place.
func (l *List) Replace(set *Set) {
	l.mu.Lock()
	l.set = set
	fns := l.onChange
	l.mu.Unlock()
	// Without the lock: a function may call Lookup and OnChange.
	for _, fn := range fns {
		fn()
	}
}

// OnChange registers fn to be called after every Replace.
func (l *List) OnChange(fn func()) {
	l.mu.Lock()
	l.onChange = append(l.onChange, fn)
	l.mu.Unlock()
}
