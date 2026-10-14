// Package keys is the list of accepted keys: the file in OpenSSH's
// authorized_keys format that says whose SSH key kosh accepts, and
// whether as an admin or read-only.
//
// A line is a key and, after it, a comment that is the key's name. The
// name decides what the key may do: one that ends in /admin is an admin,
// who may do everything kosh does; any other is read-only. A line with
// an option, and a line that does not parse, is rejected: it gives no
// access. Empty lines and lines that begin with # are skipped.
package keys

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

// AdminSuffix ends the name of a key that is an admin. It is this, in
// this case, at the very end of the name.
const AdminSuffix = "/admin"

// Grant is what the key list says about one key.
type Grant struct {
	// Name is the key's comment, or its fingerprint when it has none. An
	// admin's ends in AdminSuffix.
	Name string
	// Fingerprint is the SHA256 fingerprint of the key, as ssh-keygen -l
	// prints it: "SHA256:…".
	Fingerprint string
	// ReadOnly is true for a key that may change nothing and read no
	// Secret's contents: every key whose name does not end in AdminSuffix.
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
// than one line is read-only unless every line names it an admin, and has
// the name of its first line.
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
	// kosh checks no certificate's validity, principals or CA: a listed
	// certificate would keep working after it expired.
	if _, ok := key.(*ssh.Certificate); ok {
		return "certificates are not accepted: list the key itself"
	}
	if len(options) > 0 {
		// The option read-only of earlier versions gets its own words: a
		// list written for them means something else now.
		for _, o := range options {
			if strings.EqualFold(o, "read-only") {
				return reasonReadOnlyOption
			}
		}
		return "unknown option " + strings.Join(options, ", ")
	}
	admin := strings.HasSuffix(comment, AdminSuffix)
	// "dragan/Admin" is an admin written wrongly, not a read-only key that
	// nobody notices.
	if !admin && strings.HasSuffix(strings.ToLower(comment), AdminSuffix) {
		return reasonAdminCase
	}
	wire := string(key.Marshal())
	if g, ok := s.grants[wire]; ok {
		g.ReadOnly = g.ReadOnly || !admin
		s.grants[wire] = g
		return ""
	}
	fp := ssh.FingerprintSHA256(key)
	name := comment
	if name == "" {
		name = fp
	}
	s.grants[wire] = Grant{Name: name, Fingerprint: fp, ReadOnly: !admin}
	return ""
}

// reasonReadOnlyOption is why a line with the option read-only of kosh
// before 0.3.0 is rejected.
const reasonReadOnlyOption = "the option read-only is gone: a key is read-only unless its name ends in " + AdminSuffix

// reasonAdminCase is why a line whose name ends in /admin in another
// case is rejected.
const reasonAdminCase = "the admin suffix is " + AdminSuffix + ", in lower case, at the end of the name"

// Lookup returns the grant of key.
func (s *Set) Lookup(key ssh.PublicKey) (Grant, bool) {
	if s == nil || key == nil {
		return Grant{}, false
	}
	g, ok := s.grants[string(key.Marshal())]
	return g, ok
}

// counts returns how many keys are admins and how many read-only.
func (s *Set) counts() (admin, readOnly int) {
	if s == nil {
		return 0, 0
	}
	for _, g := range s.grants {
		if g.ReadOnly {
			readOnly++
		} else {
			admin++
		}
	}
	return admin, readOnly
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

	// What Open gives Watch: the file, the log, and the hash of the
	// content the set was read from. They do not change after Open.
	path string
	log  *slog.Logger
	sum  [sha256.Size]byte

	reads atomic.Uint64 // the readings Watch has done, for the tests
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
	data, err := os.ReadFile(path)
	if err != nil {
		// The error of os.ReadFile names the path.
		return nil, fmt.Errorf("reading the key list: %w", err)
	}
	l := NewList(parseLogged(path, data, log, "key list read"))
	l.path, l.log, l.sum = path, log, sha256.Sum256(data)
	return l, nil
}

// msgNoAdmin is logged for a list of keys none of which is an admin.
const msgNoAdmin = "key list has no admin: every session is read-only"

// parseLogged parses data, the content of the file at path, and logs
// what Open and Watch log of it: each rejected line, a list of no key, a
// list of no admin, and msg with the counts: of keys, admins and
// read-only, and of lines rejected.
func parseLogged(path string, data []byte, log *slog.Logger, msg string) *Set {
	set, rejected := Parse(bytes.NewReader(data))
	for _, r := range rejected {
		log.Warn("key list: a line is rejected and gives no access", "path", path, "line", r.Line, "reason", r.Reason)
	}
	admin, readOnly := set.counts()
	switch {
	case set.Len() == 0:
		log.Warn("key list holds no key: nobody is accepted", "path", path)
	case admin == 0:
		log.Warn(msgNoAdmin, "path", path, "hint", "an admin's name ends in "+AdminSuffix)
	}
	log.Info(msg, "path", path, "keys", set.Len(), "admin", admin, "read_only", readOnly, "rejected", len(rejected))
	return set
}

// Watch reads the file Open was given again every interval until ctx is
// done, and replaces the set when the file's content has changed. A file
// that cannot be read then is an empty set, until it can be read again.
// Watch returns when ctx is done. It is for a list that Open returned.
func (l *List) Watch(ctx context.Context, interval time.Duration) {
	// A list without a path has no file to read, and an interval of zero
	// or less is no interval: time.NewTicker would panic on it.
	if l.path == "" || interval <= 0 {
		return
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	last, readable := l.sum, true
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		// By its name each time: a ConfigMap changes by a symbolic link
		// that is swapped, and the file read last is then another one.
		data, err := os.ReadFile(l.path)
		if err != nil {
			if readable {
				readable = false
				l.log.Error("key list cannot be read: nobody is accepted until it can", "path", l.path, "err", err)
				l.Replace(&Set{})
			}
		} else if sum := sha256.Sum256(data); !readable || sum != last {
			readable, last = true, sum
			l.Replace(parseLogged(l.path, data, l.log, "key list changed"))
		}
		l.reads.Add(1)
	}
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
