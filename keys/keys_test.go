package keys

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"golang.org/x/crypto/ssh"
)

// newKey returns a fresh ed25519 public key and its authorized_keys form,
// "ssh-ed25519 AAAA…", without options, comment or line feed.
func newKey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return sshKey(t, pub)
}

func sshKey(t *testing.T, pub crypto.PublicKey) (ssh.PublicKey, string) {
	t.Helper()
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return key, strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(key)), "\n")
}

func parseString(s string) (*Set, []Rejected) {
	return Parse(strings.NewReader(s))
}

func mustGrant(t *testing.T, set *Set, key ssh.PublicKey) Grant {
	t.Helper()
	g, ok := set.Lookup(key)
	if !ok {
		t.Fatalf("key %s not in the set", ssh.FingerprintSHA256(key))
	}
	return g
}

func mustNotGrant(t *testing.T, set *Set, key ssh.PublicKey) {
	t.Helper()
	if g, ok := set.Lookup(key); ok {
		t.Fatalf("key %s is in the set: %+v", ssh.FingerprintSHA256(key), g)
	}
}

func noRejected(t *testing.T, rejected []Rejected) {
	t.Helper()
	if len(rejected) != 0 {
		t.Fatalf("rejected: %+v", rejected)
	}
}

func clusterRole(name string) Role { return Role{Kind: "ClusterRole", Name: name} }

func nsRole(namespace, name string) Role {
	return Role{Kind: "Role", Namespace: namespace, Name: name}
}

func TestParseGrants(t *testing.T) {
	alice, aliceLine := newKey(t)
	carol, carolLine := newKey(t)
	set, rejected := parseString(aliceLine + " alice@mac/cluster-admin\n" + carolLine + " carol/edit@team-a\n")
	noRejected(t, rejected)
	if set.Len() != 2 {
		t.Fatalf("Len = %d, want 2", set.Len())
	}
	want := Grant{Name: "alice@mac/cluster-admin", Fingerprint: ssh.FingerprintSHA256(alice), Role: clusterRole("cluster-admin")}
	if g := mustGrant(t, set, alice); g != want {
		t.Errorf("alice = %+v, want %+v", g, want)
	}
	want = Grant{Name: "carol/edit@team-a", Fingerprint: ssh.FingerprintSHA256(carol), Role: nsRole("team-a", "edit")}
	if g := mustGrant(t, set, carol); g != want {
		t.Errorf("carol = %+v, want %+v", g, want)
	}
}

// The role is after the name's last /: a ClusterRole, or a Role in the
// namespace after its @. What is not a role a Kubernetes API takes, and a
// name without a role or a role without a name, gives no access.
func TestParseRoleForms(t *testing.T) {
	for _, tc := range []struct {
		comment string
		name    string
		role    Role
		reason  string // of a rejection
	}{
		{comment: "a/cluster-admin", name: "a", role: clusterRole("cluster-admin")},
		{comment: "bob@laptop/view", name: "bob@laptop", role: clusterRole("view")},
		{comment: "carol/edit@team-a", name: "carol", role: nsRole("team-a", "edit")},
		{comment: "mic92/key-1/cluster-admin", name: "mic92/key-1", role: clusterRole("cluster-admin")},
		{comment: "Dragan's  old laptop/view", name: "Dragan's  old laptop", role: clusterRole("view")},
		{comment: "dragan /view", name: "dragan ", role: clusterRole("view")},
		{comment: "a/system:aggregate-to-view", name: "a", role: clusterRole("system:aggregate-to-view")},
		{comment: "a/admin", name: "a", role: clusterRole("admin")},
		{comment: "a/Admin", name: "a", role: clusterRole("Admin")},
		{comment: "alice/view  ", name: "alice", role: clusterRole("view")},
		{comment: " \talice/edit@team-a \t", name: "alice", role: nsRole("team-a", "edit")},

		{comment: "", reason: reasonNoComment},
		{comment: "  ", reason: reasonNoComment},
		{comment: "/view", reason: reasonNoName},
		{comment: "/edit@team-a", reason: reasonNoName},
		{comment: "alice", reason: reasonNoRole},
		{comment: "alice/", reason: reasonNoRole},
		{comment: "/", reason: reasonNoRole},
		{comment: "alice/@team-a", reason: reasonNoRole},
		{comment: "dragan\u2215admin", reason: reasonNoRole},
		{comment: "a/role@ns@x", reason: `the role "role@ns@x" has more than one @`},
		{comment: "alice/a b", reason: `the role "a b" holds a space`},
		{comment: "dragan/admin laptop", reason: `the role "admin laptop" holds a space`},
		{comment: "alice/..", reason: reasonBadRole},
		{comment: "alice/.", reason: reasonBadRole},
		{comment: "alice/v%w", reason: reasonBadRole},
		{comment: "alice/edit@Team", reason: reasonBadNamespace},
		{comment: "alice/edit@team.a", reason: reasonBadNamespace},
		{comment: "alice/edit@" + strings.Repeat("n", 64), reason: reasonBadNamespace},
		{comment: "alice/edit@", reason: `the role "edit" has no namespace after its @`},

		// A character that does not show, anywhere in the name, could
		// show one role and grant another.
		{comment: "bob/view\x1b[8m/cluster-admin", reason: reasonHidden},
		{comment: "bob/view\rx/cluster-admin", reason: reasonHidden},
		{comment: "x\u202bview\u200f/cluster-admin", reason: reasonHidden},
		{comment: "bob\u200b/view", reason: reasonHidden},
		{comment: "bob/vi\u200bew", reason: reasonHidden},
		{comment: "dragan/admin\u200b", reason: reasonHidden},
		{comment: "bob\tlaptop/view", reason: reasonHidden},
		{comment: "bob\u2028/view", reason: reasonHidden},
		{comment: "bob/edit@team-a\u00a0x", reason: reasonHidden},
	} {
		t.Run(strconv.Quote(tc.comment), func(t *testing.T) {
			name, role, reason := ParseRole(tc.comment)
			if tc.reason == "" {
				if reason != "" {
					t.Fatalf("rejected: %s", reason)
				}
				if name != tc.name || role != tc.role {
					t.Errorf("= %q, %+v; want %q, %+v", name, role, tc.name, tc.role)
				}
				return
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
			if name != "" || role != (Role{}) {
				t.Errorf("a rejection with %q, %+v", name, role)
			}
			// The line of the key list is rejected with the same reason.
			key, line := newKey(t)
			set, rejected := parseString(line + " " + tc.comment + "\n")
			if len(rejected) != 1 || rejected[0].Line != 1 || rejected[0].Reason != reason {
				t.Errorf("Parse rejected %+v, want line 1: %s", rejected, reason)
			}
			mustNotGrant(t, set, key)
		})
	}
}

func TestRoleNames(t *testing.T) {
	for _, tc := range []struct {
		role          Role
		group, string string
	}{
		{clusterRole("view"), "kosh:clusterrole:view", "ClusterRole/view"},
		{clusterRole("system:aggregate-to-view"), "kosh:clusterrole:system:aggregate-to-view", "ClusterRole/system:aggregate-to-view"},
		{nsRole("team-a", "edit"), "kosh:role:team-a:edit", "Role/team-a/edit"},
	} {
		if got := tc.role.Group(); got != tc.group {
			t.Errorf("%+v: Group = %q, want %q", tc.role, got, tc.group)
		}
		if got := tc.role.String(); got != tc.string {
			t.Errorf("%+v: String = %q, want %q", tc.role, got, tc.string)
		}
	}
	g := Grant{Name: "bob@laptop/view", Role: clusterRole("view")}
	if got := g.User(); got != "kosh:bob@laptop/view" {
		t.Errorf("User = %q, want kosh:bob@laptop/view", got)
	}
}

// A key without a comment has no name, and so no role: it was a read-only
// key named by its fingerprint before 0.5.0.
func TestNoCommentIsRejected(t *testing.T) {
	for _, after := range []string{"", "   ", "\t"} {
		key, line := newKey(t)
		set, rejected := parseString(line + after + "\n")
		if len(rejected) != 1 || rejected[0].Line != 1 || rejected[0].Reason != reasonNoComment {
			t.Errorf("%q: rejected = %+v, want line 1: %s", after, rejected, reasonNoComment)
		}
		mustNotGrant(t, set, key)
	}
}

// One key is one user: listed again with its role it is the key of its
// first line; with another role, the later line is rejected.
func TestDuplicateKeyWithAnotherRoleIsRejected(t *testing.T) {
	t.Run("the same role twice", func(t *testing.T) {
		key, line := newKey(t)
		set, rejected := parseString(line + " first/view\n" + line + " second/view\n")
		noRejected(t, rejected)
		if set.Len() != 1 {
			t.Errorf("Len = %d, want 1", set.Len())
		}
		if g := mustGrant(t, set, key); g.Name != "first/view" || g.Role != clusterRole("view") {
			t.Errorf("grant = %+v, want first/view", g)
		}
	})
	for _, tc := range []struct{ name, first, second string }{
		{"another ClusterRole", "first/view", "second/cluster-admin"},
		{"a Role of the same name", "first/edit", "second/edit@team-a"},
		{"the Role in another namespace", "first/edit@team-a", "second/edit@team-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, line := newKey(t)
			set, rejected := parseString("# keys\n" + line + " " + tc.first + "\n" + line + " " + tc.second + "\n")
			const want = "the key is on line 2 with another role"
			if len(rejected) != 1 || rejected[0].Line != 3 || rejected[0].Reason != want {
				t.Fatalf("rejected = %+v, want line 3: %s", rejected, want)
			}
			if g := mustGrant(t, set, key); g.Name != tc.first {
				t.Errorf("grant = %+v, want %s", g, tc.first)
			}
		})
	}
	t.Run("after a line that is rejected", func(t *testing.T) {
		key, line := newKey(t)
		set, rejected := parseString(line + " /view\n" + line + " second/cluster-admin\n")
		if len(rejected) != 1 || rejected[0].Line != 1 {
			t.Fatalf("rejected = %+v, want line 1", rejected)
		}
		if g := mustGrant(t, set, key); g.Name != "second/cluster-admin" {
			t.Errorf("grant = %+v, want second/cluster-admin", g)
		}
	})
}

// The option read-only of kosh before 0.3.0 is refused with its own words,
// so that a list written for it is not read as something else unsaid.
func TestReadOnlyOptionStillRejected(t *testing.T) {
	for _, opt := range []string{"read-only", "Read-Only", "READ-ONLY", "read-only,no-pty"} {
		t.Run(opt, func(t *testing.T) {
			key, line := newKey(t)
			set, rejected := parseString(opt + " " + line + " guest/view\n")
			if len(rejected) != 1 || rejected[0].Line != 1 || rejected[0].Reason != reasonReadOnlyOption {
				t.Fatalf("rejected = %+v, want line 1: %s", rejected, reasonReadOnlyOption)
			}
			mustNotGrant(t, set, key)
		})
	}
}

func TestSetRoles(t *testing.T) {
	var list strings.Builder
	for _, name := range []string{
		"a/view", "b/view", "c/edit@team-b", "d/edit@team-a", "e/cluster-admin", "f/admin@team-a", "g/edit",
	} {
		_, line := newKey(t)
		list.WriteString(line + " " + name + "\n")
	}
	set, rejected := parseString(list.String())
	noRejected(t, rejected)
	want := []Role{
		clusterRole("cluster-admin"), clusterRole("edit"), clusterRole("view"),
		nsRole("team-a", "admin"), nsRole("team-a", "edit"), nsRole("team-b", "edit"),
	}
	if got := set.Roles(); !slices.Equal(got, want) {
		t.Errorf("Roles = %+v\nwant %+v", got, want)
	}
	for name, set := range map[string]*Set{"nil": nil, "zero": {}} {
		if got := set.Roles(); len(got) != 0 {
			t.Errorf("%s set: Roles = %+v", name, got)
		}
	}
}

func TestParseSkipsCommentsAndBlankLines(t *testing.T) {
	_, line := newKey(t)
	set, rejected := parseString("# a comment\n\n   \n\t\n  # indented comment " + line + "\n#" + line + "\n")
	noRejected(t, rejected)
	if set.Len() != 0 {
		t.Fatalf("Len = %d, want 0", set.Len())
	}
}

func TestParseRejectsUnknownOptions(t *testing.T) {
	for _, tc := range []struct{ options, named string }{
		{"readonly", "readonly"},
		{"no-pty", "no-pty"},
		{`command="echo hi"`, `command="echo hi"`},
		{"restrict", "restrict"},
	} {
		t.Run(tc.options, func(t *testing.T) {
			key, line := newKey(t)
			set, rejected := parseString(tc.options + " " + line + " someone/view\n")
			if len(rejected) != 1 {
				t.Fatalf("rejected = %+v, want one", rejected)
			}
			if rejected[0].Line != 1 {
				t.Errorf("Line = %d, want 1", rejected[0].Line)
			}
			if !strings.Contains(rejected[0].Reason, tc.named) {
				t.Errorf("reason %q does not name %s", rejected[0].Reason, tc.named)
			}
			mustNotGrant(t, set, key)
			if set.Len() != 0 {
				t.Errorf("Len = %d, want 0", set.Len())
			}
		})
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	good, goodLine := newKey(t)
	_, other := newKey(t)
	truncated := other[:len("ssh-ed25519 ")+20]
	set, rejected := parseString("not a key\n" + truncated + "\n" + goodLine + " good/view\n")
	if len(rejected) != 2 || rejected[0].Line != 1 || rejected[1].Line != 2 {
		t.Fatalf("rejected = %+v, want lines 1 and 2", rejected)
	}
	for _, r := range rejected {
		if r.Reason == "" {
			t.Errorf("line %d rejected without a reason", r.Line)
		}
	}
	mustGrant(t, set, good)
	if set.Len() != 1 {
		t.Errorf("Len = %d, want 1", set.Len())
	}
}

func TestParseLineNumbers(t *testing.T) {
	_, line := newKey(t)
	set, rejected := parseString("# keys\n\n" + line + " a/view\n   \n# next\nno-pty " + line + " a/view\n")
	if len(rejected) != 1 || rejected[0].Line != 6 {
		t.Fatalf("rejected = %+v, want line 6", rejected)
	}
	if set.Len() != 1 {
		t.Errorf("Len = %d, want 1", set.Len())
	}
}

func TestParseCommentWithSpaces(t *testing.T) {
	key, line := newKey(t)
	set, rejected := parseString(line + "   Dragan's  old laptop/view  \n")
	noRejected(t, rejected)
	if g := mustGrant(t, set, key); g.Name != "Dragan's  old laptop/view" {
		t.Errorf("Name = %q", g.Name)
	}
}

func TestParseAsPeopleWriteThem(t *testing.T) {
	t.Run("BOM", func(t *testing.T) {
		key, line := newKey(t)
		set, rejected := parseString("\xef\xbb\xbf" + line + " bom/view\n")
		noRejected(t, rejected)
		if g := mustGrant(t, set, key); g.Name != "bom/view" {
			t.Errorf("Name = %q", g.Name)
		}
	})
	t.Run("BOM before a comment", func(t *testing.T) {
		set, rejected := parseString("\xef\xbb\xbf# keys\n")
		noRejected(t, rejected)
		if set.Len() != 0 {
			t.Errorf("Len = %d", set.Len())
		}
	})
	t.Run("CRLF", func(t *testing.T) {
		k1, l1 := newKey(t)
		k2, l2 := newKey(t)
		set, rejected := parseString("# keys\r\n\r\n" + l1 + " one/cluster-admin\r\n" + l2 + " two/edit@team-a\r\n")
		noRejected(t, rejected)
		if g := mustGrant(t, set, k1); g.Name != "one/cluster-admin" || g.Role != clusterRole("cluster-admin") {
			t.Errorf("first = %+v", g)
		}
		if g := mustGrant(t, set, k2); g.Name != "two/edit@team-a" || g.Role != nsRole("team-a", "edit") {
			t.Errorf("second = %+v", g)
		}
	})
	t.Run("no final line feed", func(t *testing.T) {
		k1, l1 := newKey(t)
		k2, l2 := newKey(t)
		set, rejected := parseString(l1 + " one/cluster-admin\n" + l2 + " two/view")
		noRejected(t, rejected)
		mustGrant(t, set, k1)
		if g := mustGrant(t, set, k2); g.Name != "two/view" || g.Role != clusterRole("view") {
			t.Errorf("last = %+v", g)
		}
	})
	t.Run("a 100 kB line", func(t *testing.T) {
		before, beforeLine := newKey(t)
		after, afterLine := newKey(t)
		long := beforeLine + " " + strings.Repeat("x", 100_000)
		set, rejected := parseString(beforeLine + " before/view\n" + long + "\n" + afterLine + " after/view\n")
		if len(rejected) != 1 || rejected[0].Line != 2 {
			t.Fatalf("rejected = %+v, want line 2", rejected)
		}
		if !strings.Contains(rejected[0].Reason, "too long") {
			t.Errorf("reason = %q, want too long", rejected[0].Reason)
		}
		mustGrant(t, set, before)
		mustGrant(t, set, after)
		if set.Len() != 2 {
			t.Errorf("Len = %d, want 2", set.Len())
		}
	})
	t.Run("a long line at the end without a line feed", func(t *testing.T) {
		_, line := newKey(t)
		set, rejected := parseString(line + " a/view\n" + line + " " + strings.Repeat("x", 100_000))
		if len(rejected) != 1 || rejected[0].Line != 2 || !strings.Contains(rejected[0].Reason, "too long") {
			t.Fatalf("rejected = %+v, want line 2 too long", rejected)
		}
		if set.Len() != 1 {
			t.Errorf("Len = %d, want 1", set.Len())
		}
	})
}

func TestParseKeyTypes(t *testing.T) {
	edPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rs, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	for name, pub := range map[string]crypto.PublicKey{
		"ed25519": edPub, "ecdsa P-256": &ec.PublicKey, "rsa 2048": &rs.PublicKey,
	} {
		t.Run(name, func(t *testing.T) {
			key, line := sshKey(t, pub)
			set, rejected := parseString(line + " " + name + "/view\n")
			noRejected(t, rejected)
			mustGrant(t, set, key)
		})
	}
}

func TestParseReaderError(t *testing.T) {
	k1, l1 := newKey(t)
	k2, l2 := newKey(t)
	k3, l3 := newKey(t)
	failure := errors.New("disk on fire")
	r := io.MultiReader(strings.NewReader(l1+" one/view\n"+l2+" two/view\n"+l3[:20]), iotest.ErrReader(failure))
	set, rejected := Parse(r)
	mustGrant(t, set, k1)
	mustGrant(t, set, k2)
	mustNotGrant(t, set, k3)
	if len(rejected) != 1 {
		t.Fatalf("rejected = %+v, want one", rejected)
	}
	if rejected[0].Line != 3 || !strings.Contains(rejected[0].Reason, "disk on fire") {
		t.Errorf("rejected = %+v, want line 3 with the error", rejected[0])
	}
}

// newCertificate returns the authorized_keys form of a user certificate
// for key, valid now and forever, signed by a fresh CA.
func newCertificate(t *testing.T, key ssh.PublicKey) string {
	t.Helper()
	_, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := ssh.NewSignerFromKey(caPriv)
	if err != nil {
		t.Fatal(err)
	}
	cert := &ssh.Certificate{
		Key:             key,
		Serial:          1,
		CertType:        ssh.UserCert,
		KeyId:           "guest",
		ValidPrincipals: []string{"guest"},
		ValidAfter:      uint64(time.Now().Add(-time.Hour).Unix()),
		ValidBefore:     ssh.CertTimeInfinity,
	}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	// The certificate is otherwise valid: its CA's signature checks out.
	checker := &ssh.CertChecker{IsUserAuthority: func(auth ssh.PublicKey) bool {
		return string(auth.Marshal()) == string(ca.PublicKey().Marshal())
	}}
	if err := checker.CheckCert("guest", cert); err != nil {
		t.Fatalf("the test's certificate is not valid: %v", err)
	}
	return strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(cert)), "\n")
}

func TestParseRejectsCertificates(t *testing.T) {
	const reason = "certificates are not accepted: list the key itself"
	for _, comment := range []string{"guest/view", "guest/cluster-admin"} {
		t.Run(comment, func(t *testing.T) {
			key, _ := newKey(t)
			line := newCertificate(t, key)
			if !strings.HasPrefix(line, "ssh-ed25519-cert-v01@openssh.com ") {
				t.Fatalf("not a certificate line: %.60s", line)
			}
			set, rejected := parseString(line + " " + comment + "\n")
			if len(rejected) != 1 || rejected[0].Line != 1 || rejected[0].Reason != reason {
				t.Fatalf("rejected = %+v, want line 1: %s", rejected, reason)
			}
			if set.Len() != 0 {
				t.Errorf("Len = %d, want 0", set.Len())
			}
			mustNotGrant(t, set, key)
		})
	}
}

func TestParseCommaOnlyOptionsIsAccepted(t *testing.T) {
	key, line := newKey(t)
	set, rejected := parseString(", " + line + " c/cluster-admin\n")
	noRejected(t, rejected)
	if g := mustGrant(t, set, key); g.Role != clusterRole("cluster-admin") || g.Name != "c/cluster-admin" {
		t.Errorf("grant = %+v, want c/cluster-admin", g)
	}
}

func TestNilSetHoldsNothing(t *testing.T) {
	key, _ := newKey(t)
	for name, set := range map[string]*Set{"nil": nil, "zero": {}} {
		if _, ok := set.Lookup(key); ok {
			t.Errorf("%s set: Lookup found a key", name)
		}
		if n := set.Len(); n != 0 {
			t.Errorf("%s set: Len = %d", name, n)
		}
	}
	_, line := newKey(t)
	set, _ := parseString(line + " a/view\n")
	if _, ok := set.Lookup(nil); ok {
		t.Error("Lookup(nil) found a key")
	}
}

func TestListReplaceCallsOnChangeAfterTheSetIsInPlace(t *testing.T) {
	key, line := newKey(t)
	l := NewList(nil)
	if _, ok := l.Lookup(key); ok {
		t.Fatal("NewList(nil) holds a key")
	}
	var calls []string
	l.OnChange(func() {
		_, ok := l.Lookup(key)
		if !ok {
			t.Error("OnChange: the new key is not found")
		}
		calls = append(calls, "first")
	})
	l.OnChange(func() { calls = append(calls, "second") })
	set, _ := parseString(line + " a/view\n")
	l.Replace(set)
	if strings.Join(calls, ",") != "first,second" {
		t.Errorf("calls = %v", calls)
	}
	if _, ok := l.Lookup(key); !ok {
		t.Error("the new key is not found after Replace")
	}
}

// TestOnChangeOrder pins what kosh serve relies on: the functions are
// called in the order they were registered, at every Replace, so that the
// bindings are made before the sessions end and deleted after.
func TestOnChangeOrder(t *testing.T) {
	_, line := newKey(t)
	l := NewList(nil)
	var calls []string
	for _, name := range []string{"make", "end", "prune"} {
		l.OnChange(func() { calls = append(calls, name) })
	}
	set, _ := parseString(line + " a/view\n")
	l.Replace(set)
	l.Replace(nil)
	if got := strings.Join(calls, ","); got != "make,end,prune,make,end,prune" {
		t.Errorf("calls = %s", got)
	}
}

func TestListRoles(t *testing.T) {
	_, a := newKey(t)
	_, b := newKey(t)
	set, rejected := parseString(a + " a/view\n" + b + " b/edit@team-a\n")
	noRejected(t, rejected)
	l := NewList(set)
	if got, want := l.Roles(), []Role{clusterRole("view"), nsRole("team-a", "edit")}; !slices.Equal(got, want) {
		t.Errorf("Roles = %+v, want %+v", got, want)
	}
	// What Roles reads is the set Replace put in place, from within
	// OnChange too.
	var inChange []Role
	l.OnChange(func() { inChange = l.Roles() })
	l.Replace(nil)
	if got := l.Roles(); len(got) != 0 || len(inChange) != 0 {
		t.Errorf("after Replace(nil): Roles = %+v, in OnChange %+v", got, inChange)
	}
}

func TestListOnChangeMayUseTheList(t *testing.T) {
	key, line := newKey(t)
	l := NewList(nil)
	var later atomic.Int32
	l.OnChange(func() {
		l.Lookup(key)
		l.OnChange(func() { later.Add(1) })
	})
	set, _ := parseString(line + " a/view\n")
	done := make(chan struct{})
	go func() {
		defer close(done)
		l.Replace(set)
		l.Replace(set)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Replace deadlocked")
	}
	if later.Load() == 0 {
		t.Error("a function registered from OnChange was not called by the next Replace")
	}
}

func TestListConcurrentUse(t *testing.T) {
	key, line := newKey(t)
	full, _ := parseString(line + " me/cluster-admin\n")
	view, _ := parseString(line + " me/view\n")
	l := NewList(full)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for range 100 {
				if _, ok := l.Lookup(key); !ok {
					t.Error("key lost")
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for j := range 50 {
				if (i+j)%2 == 0 {
					l.Replace(view)
				} else {
					l.Replace(full)
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 10 {
				l.OnChange(func() { l.Lookup(key) })
			}
		}()
	}
	wg.Wait()
}
