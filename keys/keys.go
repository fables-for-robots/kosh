// Package keys is the list of accepted keys: the file in OpenSSH's
// authorized_keys format that says whose SSH key kosh accepts, and as
// which Kubernetes role.
//
// A line is a key and, after it, a comment that is the key's name. The
// name ends in the role whose rights a session of the key has: after its
// last /, "view" is the ClusterRole view and "edit@team-a" the Role edit
// in the namespace team-a. A line without a name or a role, with a role
// or namespace that Kubernetes would not take, with an option, or that
// does not parse, is rejected: it gives no access. Empty lines and lines
// that begin with # are skipped.
package keys

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
	apipath "k8s.io/apimachinery/pkg/api/validation/path"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Grant is what the key list says about one key.
type Grant struct {
	// Name is the key's comment, the whole of it: "bob@laptop/view".
	Name string
	// Fingerprint is the SHA256 fingerprint of the key, as ssh-keygen -l
	// prints it: "SHA256:…".
	Fingerprint string
	// Role is the role the name ends in, whose rights a session has.
	Role Role
}

// User returns the user a session of the grant impersonates:
// "kosh:" + Name.
func (g Grant) User() string { return "kosh:" + g.Name }

// The kinds of Role.
const (
	KindClusterRole = "ClusterRole"
	KindRole        = "Role"
)

// Role is the Kubernetes role a key's name ends in.
type Role struct {
	// Kind is KindClusterRole or KindRole.
	Kind string
	// Namespace is set for a Role, empty for a ClusterRole.
	Namespace string
	Name      string
}

// Group returns the group that carries the role's binding:
// "kosh:clusterrole:<role>" or "kosh:role:<namespace>:<role>".
func (r Role) Group() string {
	if r.Kind == KindRole {
		return "kosh:role:" + r.Namespace + ":" + r.Name
	}
	return "kosh:clusterrole:" + r.Name
}

// String returns the role as kosh's log shows it: "ClusterRole/view" or
// "Role/team-a/edit".
func (r Role) String() string {
	if r.Kind == KindRole {
		return r.Kind + "/" + r.Namespace + "/" + r.Name
	}
	return r.Kind + "/" + r.Name
}

// Why a comment names no key: it is empty, it has no role after a /, or
// nothing before it.
const (
	reasonNoComment = "no name, no role"
	reasonNoRole    = "the name does not end in /<role> or /<role>@<namespace>"
	reasonNoName    = "a key needs a name before its role"
)

// ParseRole splits a key's comment, without the space around it, into the
// name before its last / and the role after it. A comment that names no
// role, or one Kubernetes would not take, returns why: reason is "" when
// name and role are good.
func ParseRole(comment string) (name string, role Role, reason string) {
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return "", Role{}, reasonNoComment
	}
	slash := strings.LastIndexByte(comment, '/')
	if slash < 0 {
		return "", Role{}, reasonNoRole
	}
	name, last := comment[:slash], comment[slash+1:]
	roleName, namespace, namespaced := strings.Cut(last, "@")
	switch {
	case roleName == "":
		return "", Role{}, reasonNoRole
	case name == "":
		return "", Role{}, reasonNoName
	case strings.Contains(namespace, "@"):
		return "", Role{}, fmt.Sprintf("the role %q has more than one @", last)
	}
	// Kubernetes takes a role whose name holds a space or a character
	// that does not show; in a key's name it is a mistake, such as a
	// comment that goes on after the role.
	if strings.ContainsFunc(roleName, func(r rune) bool { return r == ' ' || !unicode.IsPrint(r) }) {
		return "", Role{}, fmt.Sprintf("the role %q holds a space or a character that does not show", roleName)
	}
	if errs := apipath.IsValidPathSegmentName(roleName); len(errs) > 0 {
		return "", Role{}, fmt.Sprintf("the role %q is not a name Kubernetes takes: %s", roleName, strings.Join(errs, "; "))
	}
	if !namespaced {
		return name, Role{Kind: KindClusterRole, Name: roleName}, ""
	}
	if namespace == "" {
		return "", Role{}, fmt.Sprintf("the role %q has no namespace after its @", roleName)
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return "", Role{}, fmt.Sprintf("the namespace %q is not a name Kubernetes takes: %s", namespace, strings.Join(errs, "; "))
	}
	return name, Role{Kind: KindRole, Namespace: namespace, Name: roleName}, ""
}

// Rejected is a line of the key list that gives no access, and why.
type Rejected struct {
	Line   int // counted from 1
	Reason string
}

// Set is one reading of the key list. The zero value and nil hold no key.
type Set struct {
	grants map[string]listed // by the key's wire format
}

// listed is a key's grant and the line it is on.
type listed struct {
	Grant
	line int
}

// Parse reads a key list. It does not fail: a line it cannot accept is in
// the rejected ones, and the others are in the set. A key that is on more
// than one line has the name and role of its first; a later line that
// names another role is rejected.
func Parse(r io.Reader) (*Set, []Rejected) {
	set := &Set{grants: map[string]listed{}}
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
		} else if reason := set.add(line, n); reason != "" {
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

// add puts the key of line, the n-th of the list, into s. It returns
// why the line gives no access, or "" when it does or is a line to skip.
func (s *Set) add(line []byte, n int) (reason string) {
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
	_, role, reason := ParseRole(comment)
	if reason != "" {
		return reason
	}
	wire := string(key.Marshal())
	if first, ok := s.grants[wire]; ok {
		// One key is one user, of one role.
		if first.Role != role {
			return "the key is on line " + strconv.Itoa(first.line) + " with another role"
		}
		return ""
	}
	s.grants[wire] = listed{
		Grant: Grant{Name: comment, Fingerprint: ssh.FingerprintSHA256(key), Role: role},
		line:  n,
	}
	return ""
}

// reasonReadOnlyOption is why a line with the option read-only of kosh
// before 0.3.0 is rejected.
const reasonReadOnlyOption = "the option read-only is gone: a key has the rights of the role its name ends in, such as /view"

// Lookup returns the grant of key.
func (s *Set) Lookup(key ssh.PublicKey) (Grant, bool) {
	if s == nil || key == nil {
		return Grant{}, false
	}
	g, ok := s.grants[string(key.Marshal())]
	return g.Grant, ok
}

// Roles returns the roles the keys name, each once, by kind, namespace and
// name.
func (s *Set) Roles() []Role {
	if s == nil {
		return nil
	}
	var roles []Role
	for _, g := range s.grants {
		roles = append(roles, g.Role)
	}
	slices.SortFunc(roles, func(a, b Role) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return slices.Compact(roles)
}

// counts returns how many distinct ClusterRoles and Roles the keys name.
func (s *Set) counts() (clusterRoles, roles int) {
	for _, r := range s.Roles() {
		if r.Kind == KindClusterRole {
			clusterRoles++
		} else {
			roles++
		}
	}
	return clusterRoles, roles
}

// adminKeys returns the names of the keys of the ClusterRole admin, in
// order.
func (s *Set) adminKeys() []string {
	if s == nil {
		return nil
	}
	var names []string
	for _, g := range s.grants {
		if g.Role == (Role{Kind: KindClusterRole, Name: "admin"}) {
			names = append(names, g.Name)
		}
	}
	slices.Sort(names)
	return names
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

// msgAdminRole is logged for a list with a key of the ClusterRole admin:
// a list of 0.3 or 0.4 names its admins …/admin, which meant full access.
const msgAdminRole = "a key's role is the ClusterRole admin, which is not cluster-admin: before 0.5.0 /admin meant full access"

// parseLogged parses data, the content of the file at path, and logs
// what Open and Watch log of it: each rejected line, a list of no key, the
// keys of the ClusterRole admin, and msg with the counts: of keys, of the
// ClusterRoles and Roles they name, and of lines rejected.
func parseLogged(path string, data []byte, log *slog.Logger, msg string) *Set {
	set, rejected := Parse(bytes.NewReader(data))
	for _, r := range rejected {
		log.Warn("key list: a line is rejected and gives no access", "path", path, "line", r.Line, "reason", r.Reason)
	}
	if set.Len() == 0 {
		log.Warn("key list holds no key: nobody is accepted", "path", path)
	}
	if admins := set.adminKeys(); len(admins) > 0 {
		log.Warn(msgAdminRole, "path", path, "keys", strings.Join(admins, ", "))
	}
	clusterRoles, roles := set.counts()
	log.Info(msg, "path", path, "keys", set.Len(), "clusterroles", clusterRoles, "roles", roles, "rejected", len(rejected))
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
