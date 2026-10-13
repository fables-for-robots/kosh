package state

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

func mustLoad(t *testing.T, dir string) State {
	t.Helper()
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s): %v", dir, err)
	}
	return s
}

func TestLoadCreatesBothKeys(t *testing.T) {
	dir := t.TempDir()
	mustLoad(t, dir)
	for _, name := range []string{IrohKeyFile, HostKeyFile} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %o, want 600", name, fi.Mode().Perm())
		}
	}
	iroh, _ := os.ReadFile(filepath.Join(dir, IrohKeyFile))
	if !regexp.MustCompile(`^[0-9a-f]{64}\n$`).Match(iroh) {
		t.Errorf("iroh.key = %q, want 64 hex and a line feed", iroh)
	}
	host, _ := os.ReadFile(filepath.Join(dir, HostKeyFile))
	if !bytes.HasPrefix(host, []byte("-----BEGIN OPENSSH PRIVATE KEY-----")) {
		t.Errorf("host key file does not begin with the OpenSSH header")
	}
}

func TestLoadCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "state")
	mustLoad(t, dir)
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 700", fi.Mode().Perm())
	}
}

func TestLoadIsStable(t *testing.T) {
	dir := t.TempDir()
	a, b := mustLoad(t, dir), mustLoad(t, dir)
	if a.EndpointID() != b.EndpointID() || a.HostKeyLine() != b.HostKeyLine() || a.HostKeyFingerprint() != b.HostKeyFingerprint() {
		t.Error("second Load returned other keys")
	}
	if !bytes.Equal(a.HostKeyPEM, b.HostKeyPEM) {
		t.Error("HostKeyPEM differs")
	}
}

func TestEndpointIDIsHex(t *testing.T) {
	id := mustLoad(t, t.TempDir()).EndpointID()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
		t.Errorf("EndpointID = %q", id)
	}
}

func TestHostKeyLineAndFingerprint(t *testing.T) {
	s := mustLoad(t, t.TempDir())
	line := s.HostKeyLine()
	if !strings.HasPrefix(line, "ssh-ed25519 ") || strings.ContainsAny(line, "\n\r") || strings.Count(line, " ") != 1 {
		t.Errorf("HostKeyLine = %q", line)
	}
	fp := s.HostKeyFingerprint()
	if !strings.HasPrefix(fp, "SHA256:") {
		t.Errorf("fingerprint = %q", fp)
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if ssh.FingerprintSHA256(pub) != fp {
		t.Error("fingerprint of the line differs")
	}
}

func TestHostKeySigns(t *testing.T) {
	s := mustLoad(t, t.TempDir())
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s.HostKeyLine()))
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("hello")
	sig, err := s.HostKey.Sign(rand.Reader, msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.Verify(msg, sig); err != nil {
		t.Errorf("verify: %v", err)
	}
}

func TestIrohKeyFileForms(t *testing.T) {
	const h = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	forms := map[string]string{
		"lower":  h + "\n",
		"upper":  strings.ToUpper(h) + "\n",
		"spaces": " \n\t" + h + " \r\n\n",
	}
	var want string
	for name, content := range forms {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, IrohKeyFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		id := mustLoad(t, dir).EndpointID()
		if want == "" {
			want = id
		} else if id != want {
			t.Errorf("%s: EndpointID differs", name)
		}
	}
}

func TestIrohKeyFileThatDoesNotParse(t *testing.T) {
	cases := map[string]string{
		"63 characters": strings.Repeat("a", 63) + "\n",
		"not hex":       strings.Repeat("zq", 32) + "\n",
		"zeros":         strings.Repeat("0", 64) + "\n",
		"2000 bytes":    strings.Repeat("ab", 1000),
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, IrohKeyFile)
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(dir)
			if err == nil {
				t.Fatal("no error")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error lacks the path: %v", err)
			}
			if strings.Contains(err.Error(), strings.TrimSpace(content)[:20]) {
				t.Errorf("error repeats the content: %v", err)
			}
			got, _ := os.ReadFile(path)
			if string(got) != content {
				t.Error("file was changed")
			}
		})
	}
}

func TestHostKeyFileThatDoesNotParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, HostKeyFile)
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want one naming %s", err, path)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "garbage" {
		t.Error("file was changed")
	}
}

func TestHostKeyWithPassphrase(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, HostKeyFile)
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load(dir)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v", err)
	}
}

func TestHostKeyOfAnotherType(t *testing.T) {
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(rk, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, HostKeyFile), pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	s := mustLoad(t, dir)
	if !strings.HasPrefix(s.HostKeyLine(), "ssh-rsa ") {
		t.Errorf("HostKeyLine = %.20s", s.HostKeyLine())
	}
}

func TestOneFileMissing(t *testing.T) {
	for _, name := range []string{IrohKeyFile, HostKeyFile} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			a := mustLoad(t, dir)
			if err := os.Remove(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			b := mustLoad(t, dir)
			if name == IrohKeyFile {
				if a.HostKeyLine() != b.HostKeyLine() || a.EndpointID() == b.EndpointID() {
					t.Error("wrong key kept or replaced")
				}
			} else if a.EndpointID() != b.EndpointID() || a.HostKeyLine() == b.HostKeyLine() {
				t.Error("wrong key kept or replaced")
			}
		})
	}
}

func TestReadOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	want := mustLoad(t, dir)
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	got := mustLoad(t, dir)
	if got.EndpointID() != want.EndpointID() {
		t.Error("different key")
	}
	// Removing needs write permission: lift it, remove, lower it again.
	os.Chmod(dir, 0o700)
	path := filepath.Join(dir, HostKeyFile)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o500)
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("err = %v, want one naming %s", err, path)
	}
}

func TestConcurrentLoad(t *testing.T) {
	dir := t.TempDir()
	const n = 8
	res := make([]State, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i], errs[i] = Load(dir)
		}()
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("Load %d: %v", i, errs[i])
		}
		if res[i].EndpointID() != res[0].EndpointID() || res[i].HostKeyLine() != res[0].HostKeyLine() {
			t.Errorf("Load %d returned other keys", i)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("directory holds %d entries, want 2: %v", len(entries), entries)
	}
}
