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

func TestParseAdminAndReadOnly(t *testing.T) {
	admin, adminLine := newKey(t)
	ro, roLine := newKey(t)
	set, rejected := parseString(adminLine + " dragan@mac/admin\n" + roLine + " guest@laptop\n")
	noRejected(t, rejected)
	if set.Len() != 2 {
		t.Fatalf("Len = %d, want 2", set.Len())
	}
	g := mustGrant(t, set, admin)
	if g.ReadOnly || g.Name != "dragan@mac/admin" || g.Fingerprint != ssh.FingerprintSHA256(admin) {
		t.Errorf("admin grant = %+v", g)
	}
	g = mustGrant(t, set, ro)
	if !g.ReadOnly || g.Name != "guest@laptop" || g.Fingerprint != ssh.FingerprintSHA256(ro) {
		t.Errorf("read-only grant = %+v", g)
	}
}

// The suffix is /admin, exactly, at the very end of the name. A name that
// ends in it in another case is a mistake that is pointed out, not a
// read-only key that nobody notices; anything else is a read-only key.
func TestParseAdminSuffix(t *testing.T) {
	for _, tc := range []struct {
		comment  string
		admin    bool
		rejected bool
		why      string
	}{
		{"dragan/admin", true, false, "the suffix"},
		{"mic92/key-1/admin", true, false, "after another slash"},
		{"/admin", true, false, "nothing before it"},
		{"Dragan's  old laptop/admin", true, false, "spaces in the name"},
		{"dragan /admin", true, false, "a space before the slash: it is still the end of the name"},
		{"dragan/admin  ", true, false, "trailing spaces are not part of a name"},
		{"dragan", false, false, "no suffix"},
		{"admin", false, false, "the word without the slash"},
		{"dragan-admin", false, false, "with a dash"},
		{"dragan_admin", false, false, "with an underscore"},
		{"dragan@admin", false, false, "with an at"},
		{"dragan/admin/", false, false, "a slash after it"},
		{"dragan/admin laptop", false, false, "not at the end"},
		{"dragan/administrator", false, false, "a longer word"},
		{"dragan/admin\u200b", false, false, "a zero-width space after it: not the end, and not pointed out"},
		{"dragan\u2215admin", false, false, "a division slash, not /"},
		{"dragan/Admin", false, true, "in another case"},
		{"dragan/ADMIN", false, true, "in capitals"},
		{"DRAGAN/ADMIN", false, true, "all in capitals"},
	} {
		t.Run(tc.comment, func(t *testing.T) {
			comment, err := strconv.Unquote(`"` + tc.comment + `"`)
			if err != nil {
				t.Fatal(err)
			}
			key, line := newKey(t)
			set, rejected := parseString(line + " " + comment + "\n")
			if tc.rejected {
				if len(rejected) != 1 || rejected[0].Line != 1 || rejected[0].Reason != reasonAdminCase {
					t.Fatalf("%s: rejected = %+v, want line 1: %s", tc.why, rejected, reasonAdminCase)
				}
				mustNotGrant(t, set, key)
				return
			}
			noRejected(t, rejected)
			g := mustGrant(t, set, key)
			if g.ReadOnly == tc.admin {
				t.Errorf("%s: ReadOnly = %v, want %v", tc.why, g.ReadOnly, !tc.admin)
			}
			if want := strings.TrimSpace(comment); g.Name != want {
				t.Errorf("%s: Name = %q, want %q", tc.why, g.Name, want)
			}
		})
	}
}

// The option read-only of earlier versions is refused, so that a list
// written for them is not read as something else without a word: its
// keys without /admin are read-only now whatever it says.
func TestParseRejectsTheReadOnlyOption(t *testing.T) {
	for _, opt := range []string{"read-only", "Read-Only", "READ-ONLY", "read-only,no-pty"} {
		t.Run(opt, func(t *testing.T) {
			key, line := newKey(t)
			set, rejected := parseString(opt + " " + line + " guest\n")
			if len(rejected) != 1 || rejected[0].Line != 1 || rejected[0].Reason != reasonReadOnlyOption {
				t.Fatalf("rejected = %+v, want line 1: %s", rejected, reasonReadOnlyOption)
			}
			mustNotGrant(t, set, key)
		})
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
			set, rejected := parseString(tc.options + " " + line + " someone\n")
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
	set, rejected := parseString("not a key\n" + truncated + "\n" + goodLine + "\n")
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
	set, rejected := parseString("# keys\n\n" + line + "\n   \n# next\nno-pty " + line + "\n")
	if len(rejected) != 1 || rejected[0].Line != 6 {
		t.Fatalf("rejected = %+v, want line 6", rejected)
	}
	if set.Len() != 1 {
		t.Errorf("Len = %d, want 1", set.Len())
	}
}

func TestParseNameFallsBackToFingerprint(t *testing.T) {
	key, line := newKey(t)
	set, rejected := parseString(line + "\n")
	noRejected(t, rejected)
	g := mustGrant(t, set, key)
	if !strings.HasPrefix(g.Fingerprint, "SHA256:") {
		t.Errorf("Fingerprint = %q", g.Fingerprint)
	}
	if g.Name != g.Fingerprint {
		t.Errorf("Name = %q, want the fingerprint %q", g.Name, g.Fingerprint)
	}
}

func TestParseCommentWithSpaces(t *testing.T) {
	key, line := newKey(t)
	set, rejected := parseString(line + "   Dragan's  old laptop  \n")
	noRejected(t, rejected)
	if g := mustGrant(t, set, key); g.Name != "Dragan's  old laptop" {
		t.Errorf("Name = %q", g.Name)
	}
}

func TestParseDuplicateKey(t *testing.T) {
	for _, tc := range []struct{ name, first, second string }{
		{"admin then read-only", "first/admin", "second"},
		{"read-only then admin", "first", "second/admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, line := newKey(t)
			set, rejected := parseString(line + " " + tc.first + "\n" + line + " " + tc.second + "\n")
			noRejected(t, rejected)
			if set.Len() != 1 {
				t.Errorf("Len = %d, want 1", set.Len())
			}
			g := mustGrant(t, set, key)
			if !g.ReadOnly {
				t.Error("admin, want read-only")
			}
			if g.Name != tc.first {
				t.Errorf("Name = %q, want %q", g.Name, tc.first)
			}
		})
	}
	t.Run("admin twice", func(t *testing.T) {
		key, line := newKey(t)
		set, rejected := parseString(line + " first/admin\n" + line + " second/admin\n")
		noRejected(t, rejected)
		if g := mustGrant(t, set, key); g.ReadOnly || g.Name != "first/admin" {
			t.Errorf("grant = %+v, want admin named first/admin", g)
		}
	})
}

func TestParseAsPeopleWriteThem(t *testing.T) {
	t.Run("BOM", func(t *testing.T) {
		key, line := newKey(t)
		set, rejected := parseString("\xef\xbb\xbf" + line + " bom\n")
		noRejected(t, rejected)
		if g := mustGrant(t, set, key); g.Name != "bom" {
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
		set, rejected := parseString("# keys\r\n\r\n" + l1 + " one/admin\r\n" + l2 + " two\r\n")
		noRejected(t, rejected)
		if g := mustGrant(t, set, k1); g.Name != "one/admin" || g.ReadOnly {
			t.Errorf("first = %+v", g)
		}
		if g := mustGrant(t, set, k2); g.Name != "two" || !g.ReadOnly {
			t.Errorf("second = %+v", g)
		}
	})
	t.Run("no final line feed", func(t *testing.T) {
		k1, l1 := newKey(t)
		k2, l2 := newKey(t)
		set, rejected := parseString(l1 + " one/admin\n" + l2 + " two")
		noRejected(t, rejected)
		mustGrant(t, set, k1)
		if g := mustGrant(t, set, k2); g.Name != "two" || !g.ReadOnly {
			t.Errorf("last = %+v", g)
		}
	})
	t.Run("a 100 kB line", func(t *testing.T) {
		before, beforeLine := newKey(t)
		after, afterLine := newKey(t)
		long := beforeLine + " " + strings.Repeat("x", 100_000)
		set, rejected := parseString(beforeLine + "\n" + long + "\n" + afterLine + "\n")
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
		set, rejected := parseString(line + "\n" + line + " " + strings.Repeat("x", 100_000))
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
			set, rejected := parseString(line + " " + name + "\n")
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
	r := io.MultiReader(strings.NewReader(l1+"\n"+l2+"\n"+l3[:20]), iotest.ErrReader(failure))
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
	for _, comment := range []string{"guest", "guest/admin"} {
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
	set, rejected := parseString(", " + line + " c/admin\n")
	noRejected(t, rejected)
	if g := mustGrant(t, set, key); g.ReadOnly || g.Name != "c/admin" {
		t.Errorf("grant = %+v, want an admin named c/admin", g)
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
	set, _ := parseString(line + "\n")
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
	set, _ := parseString(line + "\n")
	l.Replace(set)
	if strings.Join(calls, ",") != "first,second" {
		t.Errorf("calls = %v", calls)
	}
	if _, ok := l.Lookup(key); !ok {
		t.Error("the new key is not found after Replace")
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
	set, _ := parseString(line + "\n")
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
	full, _ := parseString(line + "\n")
	ro, _ := parseString(line + " guest\n")
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
					l.Replace(ro)
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
