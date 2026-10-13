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

func TestParseFullAndReadOnly(t *testing.T) {
	full, fullLine := newKey(t)
	ro, roLine := newKey(t)
	set, rejected := parseString(fullLine + " dragan@mac\nread-only " + roLine + " guest@laptop\n")
	noRejected(t, rejected)
	if set.Len() != 2 {
		t.Fatalf("Len = %d, want 2", set.Len())
	}
	g := mustGrant(t, set, full)
	if g.ReadOnly || g.Name != "dragan@mac" || g.Fingerprint != ssh.FingerprintSHA256(full) {
		t.Errorf("full grant = %+v", g)
	}
	g = mustGrant(t, set, ro)
	if !g.ReadOnly || g.Name != "guest@laptop" || g.Fingerprint != ssh.FingerprintSHA256(ro) {
		t.Errorf("read-only grant = %+v", g)
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
		{"read-only,no-pty", "no-pty"},
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

func TestParseReadOnlyInAnyCase(t *testing.T) {
	for _, opt := range []string{"Read-Only", "READ-ONLY"} {
		t.Run(opt, func(t *testing.T) {
			key, line := newKey(t)
			set, rejected := parseString(opt + " " + line + "\n")
			noRejected(t, rejected)
			if !mustGrant(t, set, key).ReadOnly {
				t.Errorf("%s gave a full key", opt)
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
		{"full then read-only", "", "read-only "},
		{"read-only then full", "read-only ", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, line := newKey(t)
			set, rejected := parseString(tc.first + line + " first\n" + tc.second + line + " second\n")
			noRejected(t, rejected)
			if set.Len() != 1 {
				t.Errorf("Len = %d, want 1", set.Len())
			}
			g := mustGrant(t, set, key)
			if !g.ReadOnly {
				t.Error("full, want read-only")
			}
			if g.Name != "first" {
				t.Errorf("Name = %q, want first", g.Name)
			}
		})
	}
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
		set, rejected := parseString("# keys\r\n\r\n" + l1 + " one\r\nread-only " + l2 + " two\r\n")
		noRejected(t, rejected)
		if g := mustGrant(t, set, k1); g.Name != "one" || g.ReadOnly {
			t.Errorf("first = %+v", g)
		}
		if g := mustGrant(t, set, k2); g.Name != "two" || !g.ReadOnly {
			t.Errorf("second = %+v", g)
		}
	})
	t.Run("no final line feed", func(t *testing.T) {
		k1, l1 := newKey(t)
		k2, l2 := newKey(t)
		set, rejected := parseString(l1 + " one\nread-only " + l2 + " two")
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
	ro, _ := parseString("read-only " + line + "\n")
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
