package cli

import (
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The lines kosh id prints, as a pattern: the labels padded so that the
// values start in one column.
var idLines = regexp.MustCompile(`^endpoint ID   ([0-9a-f]{64})\n` +
	`iroh-ssh      iroh-ssh kosh@([0-9a-f]{64})\n` +
	`host key      (ssh-ed25519 AAAA[0-9A-Za-z+/=]+)\n` +
	`fingerprint   (SHA256:[0-9A-Za-z+/]+)\n$`)

func TestIDPrintsFourLines(t *testing.T) {
	unsetenv(t, "KOSH_STATE_DIR")
	out, _, err := run(t, App(), "id", "--state-dir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := idLines.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("printed:\n%s", out)
	}
	if m[1] != m[2] {
		t.Errorf("endpoint ID %s, iroh-ssh's %s", m[1], m[2])
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(m[3]))
	if err != nil {
		t.Fatalf("host key line %q: %v", m[3], err)
	}
	if fp := ssh.FingerprintSHA256(key); fp != m[4] {
		t.Errorf("fingerprint line %s, the host key's %s", m[4], fp)
	}
}

func TestIDIsStable(t *testing.T) {
	unsetenv(t, "KOSH_STATE_DIR")
	dir := t.TempDir()
	first, _, err := run(t, App(), "id", "--state-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := run(t, App(), "id", "--state-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == "" {
		t.Errorf("first:\n%s\nsecond:\n%s", first, second)
	}
}

func TestIDNeedsStateDir(t *testing.T) {
	unsetenv(t, "KOSH_STATE_DIR")
	out, _, err := run(t, App(), "id")
	if err == nil || err.Error() != "--state-dir is required" {
		t.Errorf("error %v", err)
	}
	if out != "" {
		t.Errorf("printed %q", out)
	}

	t.Setenv("KOSH_STATE_DIR", t.TempDir())
	out, _, err = run(t, App(), "id")
	if err != nil {
		t.Fatal(err)
	}
	if !idLines.MatchString(out) {
		t.Errorf("printed:\n%s", out)
	}
}

func TestIDFailsOnABadStateDir(t *testing.T) {
	unsetenv(t, "KOSH_STATE_DIR")
	dir := t.TempDir()
	if _, _, err := run(t, App(), "id", "--state-dir", dir); err != nil {
		t.Fatal(err)
	}
	// A key file that does not parse is an error, and is left as it is.
	writeFile(t, dir+"/iroh.key", "not a key\n")
	out, _, err := run(t, App(), "id", "--state-dir", dir)
	if err == nil || !strings.HasPrefix(err.Error(), "the state directory ") || !strings.Contains(err.Error(), dir) {
		t.Errorf("error %v, want one that names the directory", err)
	}
	if out != "" {
		t.Errorf("printed %q", out)
	}
}
