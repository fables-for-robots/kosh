//go:build e2e

package e2e

// The tests of kosh as its users meet it. Two scripts run them and hand
// them what they need in the environment (see config):
//
//   - scripts/test-e2e.sh installs kosh's image with its chart in a kind
//     cluster, and runs every test but TestIrohSSHFindsKoshByMDNS;
//   - scripts/test-mdns.sh runs kosh as a process on the machine, with
//     the relays off, and runs TestIrohSSHFindsKoshByMDNS alone.
//
// Each test reads kosh's screen as a user would see it, through the
// screen model of screen_test.go.
//
// The tests of the cluster run in the order of this file, against one
// cluster and one kosh: TestRoleCanBeChanged, TestKeyRemovedEndsTheSession
// and TestShellEndsWithItsKey change kosh's role and its key list, and
// put them back; TestStopTellsTheSessions restarts kosh and comes last.
// The tests of a shell come before them.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

// The release and namespace scripts/test-e2e.sh installs kosh as, and
// the chart it installs.
const (
	namespace = "kosh"
	release   = "kosh"
	chart     = "../charts/kosh"
)

// wait is how long a test waits for anything that has no deadline of
// its own.
const wait = 30 * time.Second

// config is what the scripts hand the tests.
type config struct {
	sshAddr  string // KOSH_E2E_SSH: the address the Service is forwarded to
	fullKey  string // KOSH_E2E_FULL_KEY: the full key's private key file; its .pub beside it
	roKey    string // KOSH_E2E_READONLY_KEY: the read-only key's
	hostKey  string // KOSH_E2E_HOST_KEY: a file with kosh's host key, as kosh id printed it
	endpoint string // KOSH_E2E_ENDPOINT: kosh's endpoint ID
	irohSSH  string // KOSH_E2E_IROH_SSH: the iroh-ssh to run
	// logFile is kosh's log when kosh runs as a process
	// (KOSH_E2E_LOG); without it, kosh's log is the pod's.
	logFile string
	// noNetwork skips the tests that need the relays: KOSH_E2E_NO_NETWORK=1.
	noNetwork bool
	// mdns is set by scripts/test-mdns.sh: KOSH_E2E_MDNS=1.
	mdns bool
}

var cfg = config{
	sshAddr:   os.Getenv("KOSH_E2E_SSH"),
	fullKey:   os.Getenv("KOSH_E2E_FULL_KEY"),
	roKey:     os.Getenv("KOSH_E2E_READONLY_KEY"),
	hostKey:   os.Getenv("KOSH_E2E_HOST_KEY"),
	endpoint:  os.Getenv("KOSH_E2E_ENDPOINT"),
	irohSSH:   os.Getenv("KOSH_E2E_IROH_SSH"),
	logFile:   os.Getenv("KOSH_E2E_LOG"),
	noNetwork: os.Getenv("KOSH_E2E_NO_NETWORK") == "1",
	mdns:      os.Getenv("KOSH_E2E_MDNS") == "1",
}

// need fails the test unless every one of the variables is set.
func need(t *testing.T, names ...string) {
	t.Helper()
	var missing []string
	for _, n := range names {
		if os.Getenv(n) == "" {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("this test needs %s: run it with scripts/test-e2e.sh or scripts/test-mdns.sh", strings.Join(missing, ", "))
	}
}

// inCluster fails the test unless scripts/test-e2e.sh set up kosh in
// its cluster.
func inCluster(t *testing.T) {
	t.Helper()
	need(t, "KOSH_E2E_SSH", "KOSH_E2E_FULL_KEY", "KOSH_E2E_READONLY_KEY", "KOSH_E2E_HOST_KEY",
		"KOSH_E2E_ENDPOINT", "KOSH_E2E_IROH_SSH", "KUBECONFIG")
}

// A key is a client key: its signer, its file, and its name, which is
// its comment.
type key struct {
	signer ssh.Signer
	file   string
	name   string
	line   string // the public key's line, as its .pub file has it
}

func loadKey(t *testing.T, file string) key {
	t.Helper()
	priv, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(priv)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	pub, err := os.ReadFile(file + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	_, comment, _, _, err := ssh.ParseAuthorizedKey(pub)
	if err != nil {
		t.Fatalf("%s.pub: %v", file, err)
	}
	return key{signer: signer, file: file, name: comment, line: strings.TrimSpace(string(pub))}
}

func fullKey(t *testing.T) key     { return loadKey(t, cfg.fullKey) }
func readOnlyKey(t *testing.T) key { return loadKey(t, cfg.roKey) }

// hostKey is kosh's host key.
func hostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	b, err := os.ReadFile(cfg.hostKey)
	if err != nil {
		t.Fatal(err)
	}
	k, _, _, _, err := ssh.ParseAuthorizedKey(b)
	if err != nil {
		t.Fatalf("%s: %v", cfg.hostKey, err)
	}
	return k
}

// run runs a command with a deadline and returns what it printed on
// standard output, and its error with standard error in it.
func run(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// kubectl runs kubectl against the test's cluster and fails the test
// when it fails.
func kubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := run("kubectl", args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// koshLog returns kosh's log, all of it.
func koshLog(t *testing.T) string {
	t.Helper()
	if cfg.logFile != "" {
		b, err := os.ReadFile(cfg.logFile)
		if err != nil {
			return "(kosh's log could not be read: " + err.Error() + ")"
		}
		return string(b)
	}
	out, err := run("kubectl", "--namespace", namespace, "logs", "deploy/"+release)
	if err != nil {
		return "(kosh's log could not be read: " + err.Error() + ")"
	}
	return out
}

// logLines returns the lines of kosh's log that have msg and every one
// of attrs, which come in pairs of a key and a value.
func logLines(t *testing.T, msg string, attrs ...string) []string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(koshLog(t), "\n") {
		if hasAttr(line, "msg", msg) && hasAttrs(line, attrs...) {
			found = append(found, line)
		}
	}
	return found
}

func hasAttrs(line string, attrs ...string) bool {
	for i := 0; i+1 < len(attrs); i += 2 {
		if !hasAttr(line, attrs[i], attrs[i+1]) {
			return false
		}
	}
	return true
}

// hasAttr reports whether a line of slog's text handler has k=v, with v
// quoted or not.
func hasAttr(line, k, v string) bool {
	return strings.Contains(" "+line+" ", " "+k+"="+v+" ") || strings.Contains(line, " "+k+"="+strconv.Quote(v))
}

// eventually waits until cond holds, for within at most, and fails the
// test with what describe says when it does not.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool, describe func() string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not after %v.\n%s", what, within, describe())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A term is a terminal a session of kosh is drawn on: the screen, and
// how to type into it.
type term struct {
	t    *testing.T
	scr  *screen
	in   io.Writer
	what string // who is connected, how, for the failures
}

// waitUntil waits until cond holds for the screen's lines, for within
// at most, and fails the test with the screen it had when it does not.
func (c *term) waitUntil(within time.Duration, what string, cond func(lines []string) bool) {
	c.t.Helper()
	eventually(c.t, within, c.what+": "+what, func() bool { return cond(c.scr.lines()) },
		func() string { return "the screen:\n" + c.scr.String() })
}

// waitFor waits until the screen holds every one of texts.
func (c *term) waitFor(texts ...string) {
	c.t.Helper()
	c.waitUntil(wait, fmt.Sprintf("the screen shows %q", texts), func(lines []string) bool {
		all := strings.Join(lines, "\n")
		for _, s := range texts {
			if !strings.Contains(all, s) {
				return false
			}
		}
		return true
	})
}

// waitRow waits until a row of the table begins with first and holds
// every one of more.
func (c *term) waitRow(first string, more ...string) {
	c.t.Helper()
	c.waitUntil(wait, fmt.Sprintf("a row %q with %q", first, more), func(lines []string) bool {
		return row(lines, first, more...) != ""
	})
}

// row returns the row of the table whose first cell is first and that
// holds every one of more, or "".
func row(lines []string, first string, more ...string) string {
	for _, l := range lines {
		cells := strings.Fields(strings.Trim(l, "│ "))
		if len(cells) == 0 || cells[0] != first {
			continue
		}
		ok := true
		for _, m := range more {
			ok = ok && strings.Contains(l, m)
		}
		if ok {
			return l
		}
	}
	return ""
}

// lineWith returns whether a line holds every one of texts.
func lineWith(lines []string, texts ...string) bool {
	for _, l := range lines {
		ok := true
		for _, s := range texts {
			ok = ok && strings.Contains(l, s)
		}
		if ok {
			return true
		}
	}
	return false
}

// send types keys.
func (c *term) send(keys ...string) {
	c.t.Helper()
	for _, k := range keys {
		if _, err := io.WriteString(c.in, k); err != nil {
			c.t.Fatalf("%s: typing %q: %v", c.what, k, err)
		}
	}
}

// The keys, as a terminal sends them.
const (
	keyEnter     = "\r"
	keyEsc       = "\x1b"
	keyRight     = "\x1b[C"
	keyBackspace = "\x7f"
	keyCtrlK     = "\x0b"
)

// fills waits until the frame fills a terminal of w by h: the table's
// border runs from the first column to the last, and its bottom is on
// the third line from the bottom, above the crumbs and the flash.
func (c *term) fills(w, h int) {
	c.t.Helper()
	c.waitUntil(wait, fmt.Sprintf("the frame fills %dx%d", w, h), func(lines []string) bool {
		if len(lines) != h {
			return false
		}
		top := false
		for _, l := range lines {
			r := []rune(l)
			if len(r) == w && r[0] == '┌' && r[w-1] == '┐' {
				top = true
			}
		}
		bottom := []rune(lines[h-3])
		return top && len(bottom) == w && bottom[0] == '└' && bottom[w-1] == '┘' && strings.HasPrefix(lines[h-2], "<")
	})
}

// A goSession is a session of the Go SSH client over the forwarded
// port, with a terminal.
type goSession struct {
	term
	client *ssh.Client
	sess   *ssh.Session
	stderr syncBuffer
	waited chan error
}

// syncBuffer is a buffer that is written and read from two goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// dialSSH connects as k over the forwarded port and opens a session with
// a terminal of w by h. It checks the host key against kosh's.
func dialSSH(t *testing.T, k key, w, h int) (*goSession, error) {
	t.Helper()
	client, err := ssh.Dial("tcp", cfg.sshAddr, &ssh.ClientConfig{
		User:            "kosh",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(k.signer)},
		HostKeyCallback: ssh.FixedHostKey(hostKey(t)),
		Timeout:         wait,
	})
	if err != nil {
		return nil, err
	}
	c := &goSession{client: client, waited: make(chan error, 1)}
	c.term = term{t: t, scr: newScreen(w, h), what: fmt.Sprintf("ssh as %s", k.name)}
	t.Cleanup(func() { _ = client.Close() })
	if c.sess, err = client.NewSession(); err != nil {
		t.Fatal(err)
	}
	if err := c.sess.RequestPty("xterm-256color", h, w, ssh.TerminalModes{}); err != nil {
		t.Fatal(err)
	}
	stdin, err := c.sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	c.in = stdin
	c.sess.Stdout = c.scr
	c.sess.Stderr = &c.stderr
	if err := c.sess.Shell(); err != nil {
		t.Fatal(err)
	}
	go func() { c.waited <- c.sess.Wait() }()
	return c, nil
}

// connect is dialSSH that fails the test when it cannot connect.
func connect(t *testing.T, k key, w, h int) *goSession {
	t.Helper()
	c, err := dialSSH(t, k, w, h)
	if err != nil {
		t.Fatalf("connecting as %s: %v\nkosh's log:\n%s", k.name, err, koshLog(t))
	}
	return c
}

// resize changes the terminal's size, as a terminal's window does.
func (c *goSession) resize(w, h int) {
	c.t.Helper()
	c.scr.resize(w, h)
	if err := c.sess.WindowChange(h, w); err != nil {
		c.t.Fatal(err)
	}
}

// ended waits for the session to end and returns its Wait's error.
func (c *goSession) ended(within time.Duration) error {
	c.t.Helper()
	select {
	case err := <-c.waited:
		return err
	case <-time.After(within):
		c.t.Fatalf("%s: the session has not ended in %v; the screen:\n%s", c.what, within, c.scr)
		return nil
	}
}

// alive reports whether the session has not ended.
func (c *goSession) alive() bool {
	select {
	case err := <-c.waited:
		c.waited <- err
		return false
	default:
		return true
	}
}

// A ptySession is a program, ssh or iroh-ssh, run on a pseudo-terminal
// as a user runs it.
type ptySession struct {
	term
	cmd    *exec.Cmd
	ptmx   *os.File
	exited chan error
}

// startPTY starts cmd on a pseudo-terminal of w by h, with TERM set as a
// terminal emulator sets it.
func startPTY(t *testing.T, cmd *exec.Cmd, w, h int) *ptySession {
	t.Helper()
	cmd.Env = append(cmd.Environ(), "TERM=xterm-256color")
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)})
	if err != nil {
		t.Fatalf("starting %s: %v", cmd.Path, err)
	}
	p := &ptySession{cmd: cmd, ptmx: ptmx, exited: make(chan error, 1)}
	p.term = term{t: t, scr: newScreen(w, h), in: ptmx, what: filepath.Base(cmd.Path)}
	copied := make(chan struct{})
	go func() {
		_, _ = io.Copy(p.scr, ptmx) // ends with EIO once the program has exited
		close(copied)
	}()
	go func() {
		err := cmd.Wait()
		select {
		case <-copied:
		case <-time.After(time.Second):
		}
		p.exited <- err
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = ptmx.Close()
	})
	return p
}

// exitedWith waits for the program to exit and returns its status.
func (p *ptySession) exitedWith(within time.Duration) int {
	p.t.Helper()
	select {
	case err := <-p.exited:
		p.exited <- err
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			p.t.Fatalf("%s: %v", p.what, err)
		}
		return 0
	case <-time.After(within):
		p.t.Fatalf("%s has not exited in %v; the screen:\n%s", p.what, within, p.scr)
		return -1
	}
}

// knownHosts writes a known_hosts file with kosh's host key for the
// forwarded port, for the real ssh to check it against.
func knownHosts(t *testing.T) string {
	t.Helper()
	host, port, err := net.SplitHostPort(cfg.sshAddr)
	if err != nil {
		t.Fatal(err)
	}
	line := knownHostsLine(fmt.Sprintf("[%s]:%s", host, port), hostKey(t))
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func knownHostsLine(host string, k ssh.PublicKey) string {
	return host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

// sshCommand is the real ssh, to kosh over the forwarded port as k,
// with the host key checked and nothing of the user's configuration:
// ssh with options, the destination, and command.
func sshCommand(t *testing.T, k key, options []string, command ...string) *exec.Cmd {
	t.Helper()
	host, port, err := net.SplitHostPort(cfg.sshAddr)
	if err != nil {
		t.Fatal(err)
	}
	all := []string{
		"-F", "/dev/null", "-p", port, "-i", k.file,
		"-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + knownHosts(t), "-o", "GlobalKnownHostsFile=/dev/null",
	}
	all = append(all, options...)
	all = append(all, "kosh@"+host)
	all = append(all, command...)
	return exec.Command("ssh", all...)
}

// serverVersion is the API server's version, as kubectl has it.
func serverVersion(t *testing.T) string {
	t.Helper()
	var v struct {
		ServerVersion struct {
			GitVersion string `json:"gitVersion"`
		} `json:"serverVersion"`
	}
	if err := json.Unmarshal([]byte(kubectl(t, "version", "-o", "json")), &v); err != nil {
		t.Fatal(err)
	}
	return v.ServerVersion.GitVersion
}

func TestSSHShowsThePods(t *testing.T) {
	inCluster(t)
	k := fullKey(t)
	c := connect(t, k, 120, 40)
	c.waitFor("Pods(default)")
	c.waitRow("marker", "Running")
	version := serverVersion(t)
	c.waitUntil(wait, "the key's name and the cluster's version in the header", func(lines []string) bool {
		return lineWith(lines, "Key:", k.name) && lineWith(lines, "Cluster:", version)
	})
	c.fills(120, 40)

	// The frame follows the terminal's size.
	c.resize(80, 24)
	c.fills(80, 24)
	c.waitRow("marker", "Running")
	c.resize(120, 40)
	c.fills(120, 40)
}

// markerLines matches what the pod marker prints.
var markerLines = regexp.MustCompile(`kosh-e2e-marker (\d+)`)

// lastMarker is the highest number of the marker's lines on the screen,
// or -1.
func lastMarker(lines []string) int {
	last := -1
	for _, m := range markerLines.FindAllStringSubmatch(strings.Join(lines, "\n"), -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > last {
			last = n
		}
	}
	return last
}

func TestSSHLogs(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 120, 40)
	c.waitRow("marker", "Running")
	c.send("l")
	c.waitFor("kosh-e2e-marker")
	seen := lastMarker(c.scr.lines())
	c.waitUntil(5*time.Second, fmt.Sprintf("a line of the marker after kosh-e2e-marker %d", seen), func(lines []string) bool {
		return lastMarker(lines) > seen
	})
}

func TestSSHDescribeAndYAML(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 120, 40)
	c.waitRow("marker", "Running")
	c.send("d")
	c.waitUntil(wait, "the description of marker", func(lines []string) bool {
		return lineWith(lines, "Name:", "marker")
	})
	c.send(keyEsc)
	c.waitRow("marker", "Running")
	c.send("y")
	c.waitFor("kind: Pod", "name: marker")
}

// podOfWeb matches the name of a pod of the deployment web.
var podOfWeb = regexp.MustCompile(`^web-[a-z0-9]+-[a-z0-9]+$`)

// firstPodOfWeb waits for the table to show a pod of web and returns the
// first one's name.
func (c *term) firstPodOfWeb() string {
	c.t.Helper()
	name := ""
	c.waitUntil(wait, "a pod of web", func(lines []string) bool {
		for _, l := range lines {
			cells := strings.Fields(strings.Trim(l, "│ "))
			if len(cells) > 0 && podOfWeb.MatchString(cells[0]) {
				name = cells[0]
				return true
			}
		}
		return false
	})
	return name
}

func TestSSHNavigates(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 120, 40)
	c.waitFor("Pods(default)")
	c.send(":deploy" + keyEnter)
	c.waitFor("Deployments(default)")
	c.waitRow("web")
	c.send(keyEnter)
	c.firstPodOfWeb()
	c.send(keyEsc)
	c.waitFor("Deployments(default)")
	c.waitRow("web")

	c.send(":ns" + keyEnter)
	c.waitFor("Namespaces(all)")
	c.waitRow("default")
	c.waitRow("kosh")

	// A custom resource is drawn with the columns the API server's
	// table has for it.
	c.send(":widgets" + keyEnter)
	c.waitFor("Widgets(default)", "COLOR")
	c.waitRow("gizmo", "teal")
}

// replicas is the replicas of the deployment web, as the API has them.
func replicas(t *testing.T) string {
	t.Helper()
	return kubectl(t, "--namespace", "default", "get", "deploy", "web", "-o", "jsonpath={.spec.replicas}")
}

// The keys of a scale to 3 replicas, once its dialog is open: the field
// set to 3, then enter (to Cancel), right (to Scale), enter.
var (
	scaleTo3     = []string{keyBackspace, "3"}
	scaleConfirm = []string{keyEnter, keyRight, keyEnter}
)

func TestSSHScales(t *testing.T) {
	inCluster(t)
	k := fullKey(t)
	t.Cleanup(func() {
		kubectl(t, "--namespace", "default", "scale", "deploy", "web", "--replicas=1")
	})
	c := connect(t, k, 120, 40)
	c.waitFor("Pods(default)")
	c.send(":deploy" + keyEnter)
	c.waitRow("web")
	// The hint a read-only key is not shown (TestReadOnlyKey).
	c.waitUntil(wait, "the hint <s> Scale", func(lines []string) bool { return lineWith(lines, "<s> Scale") })
	c.send("s")
	c.waitFor("Scale deployments/default/web?", "Replicas")
	c.send(scaleTo3...)
	c.waitUntil(wait, "the field holds 3", func(lines []string) bool {
		return row(lines, "Replicas", "3") != "" || lineWith(lines, "Replicas", " 3 ")
	})
	c.send(scaleConfirm[0]) // from the field to Cancel
	c.waitFor("[ Cancel ]")
	c.send(scaleConfirm[1])
	c.waitFor("[ Scale ]")
	c.send(scaleConfirm[2])

	eventually(t, 10*time.Second, "the deployment web has 3 replicas", func() bool { return replicas(t) == "3" },
		func() string { return "replicas: " + replicas(t) + "\nthe screen:\n" + c.scr.String() })
	eventually(t, wait, "kosh's log has the action", func() bool {
		return len(logLines(t, "action", "key", k.name, "action", "scale", "object", "deployments/default/web")) > 0
	}, func() string { return "kosh's log:\n" + koshLog(t) })
}

// podUID returns the UID of the pod when it is there and not being
// deleted, and "" otherwise.
func podUID(t *testing.T, name string) string {
	t.Helper()
	out, err := run("kubectl", "--namespace", "default", "get", "pod", name, "--ignore-not-found",
		"-o", "jsonpath={.metadata.uid} {.metadata.deletionTimestamp}")
	if err != nil {
		t.Fatal(err)
	}
	f := strings.Fields(out)
	if len(f) != 1 {
		return ""
	}
	return f[0]
}

func TestSSHKillAsksFirst(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 120, 40)
	c.waitFor("Pods(default)")
	c.send(":deploy" + keyEnter)
	c.waitRow("web")
	c.send(keyEnter)
	pod := c.firstPodOfWeb()
	uid := podUID(t, pod)
	if uid == "" {
		t.Fatalf("the pod %s is not there, or is being deleted", pod)
	}
	question := "Kill pods/default/" + pod + "?"
	killed := func() []string { return logLines(t, "action", "action", "kill", "object", "pods/default/"+pod) }

	// The focus is on Cancel: enter at once kills nothing.
	c.send(keyCtrlK)
	c.waitFor(question, "[ Cancel ]")
	c.send(keyEnter)
	c.waitUntil(wait, "the dialog is gone", func(lines []string) bool { return !lineWith(lines, question) })
	// Nothing is to be waited for: a kill would have happened by now.
	time.Sleep(2 * time.Second)
	if got := podUID(t, pod); got != uid {
		t.Fatalf("after Cancel the pod %s (UID %s) is gone or being deleted; UID now %q", pod, uid, got)
	}
	if lines := killed(); len(lines) > 0 {
		t.Fatalf("after Cancel kosh's log has a kill: %s", lines[0])
	}

	c.send(keyCtrlK)
	c.waitFor(question, "[ Cancel ]")
	c.send(keyRight)
	c.waitFor("[ Kill ]")
	c.send(keyEnter)
	eventually(t, wait, "the pod "+pod+" is deleted", func() bool { return podUID(t, pod) == "" },
		func() string { return "the screen:\n" + c.scr.String() })
	eventually(t, wait, "kosh's log has the kill", func() bool { return len(killed()) > 0 },
		func() string { return "kosh's log:\n" + koshLog(t) })
}

// secretValue is the value of the Secret hidden.
const secretValue = "kosh-e2e-secret-value"

// secretBase64 is the Secret's value as the YAML of the Secret has it,
// and as a copy of it to the clipboard (OSC 52) would carry the text.
var secretBase64 = base64.StdEncoding.EncodeToString([]byte(secretValue))

// secretForms are the forms a leak of the Secret's value would take: as
// it is, as x shows it, and in base64; and a distinctive beginning of
// each, for a value cut by a narrow column. ("kosh-e2e-" alone is in
// other names of the test.)
var secretForms = []string{secretValue, secretBase64, secretValue[:len("kosh-e2e-secr")], secretBase64[:16]}

// noSecret fails the test when any of secretForms was ever on c's screen,
// on it with its rows joined, or in what kosh sent it, raw or with the
// escape sequences taken out, since c's screen began watching for them.
func noSecret(t *testing.T, c *term, when string) {
	t.Helper()
	if seen := c.scr.seen(); len(seen) > 0 {
		t.Fatalf("%s: the Secret's value reached the terminal: %s", when, strings.Join(seen, "\n"))
	}
}

func TestReadOnlyKey(t *testing.T) {
	inCluster(t)
	k := readOnlyKey(t)
	c := connect(t, k, 120, 40)
	c.waitUntil(wait, "the key's name with [read-only] in the header", func(lines []string) bool {
		return lineWith(lines, "Key:", k.name+" [read-only]")
	})
	// From here on, every write to the terminal is looked at for the
	// Secret's value in each of its forms, to the end of the test and
	// what is received while it ends.
	c.scr.watch(secretForms...)
	t.Cleanup(func() {
		if seen := c.scr.seen(); len(seen) > 0 {
			t.Errorf("by the end: the Secret's value reached the terminal: %s", strings.Join(seen, "\n"))
		}
	})

	c.send(":deploy" + keyEnter)
	c.waitRow("web")
	if lineWith(c.scr.lines(), "<s>", "Scale") {
		t.Errorf("a read-only key is offered <s> Scale:\n%s", c.scr)
	}
	before := replicas(t)
	// What TestSSHScales types to scale web to 3.
	c.send("s")
	c.send(scaleTo3...)
	c.send(scaleConfirm...)
	// Nothing is to be waited for: what s would open or do would be there
	// by now.
	time.Sleep(2 * time.Second)
	if lineWith(c.scr.lines(), "Replicas") || lineWith(c.scr.lines(), "Scale deployments/") {
		t.Errorf("s opened something for a read-only key:\n%s", c.scr)
	}
	if after := replicas(t); after != before {
		t.Errorf("the replicas of web went from %s to %s", before, after)
	}
	if lines := logLines(t, "action", "key", k.name, "action", "scale"); len(lines) > 0 {
		t.Errorf("kosh's log has a scale of the read-only key: %s", lines[0])
	}

	// The keys above may have gone into web's pods: name the namespace.
	c.send(":secrets default" + keyEnter)
	c.waitFor("Secrets(default)")
	c.waitRow("hidden")
	noSecret(t, &c.term, "the table of secrets")
	for _, key := range []string{"y", "d", "x"} {
		c.send(key)
		c.waitUntil(wait, key+" on a Secret is refused, and the table stays", func(lines []string) bool {
			return lineWith(lines, "this key is read-only") && lineWith(lines, "Secrets(default)") && row(lines, "hidden") != ""
		})
		noSecret(t, &c.term, key+" on the Secret")
		// An error stays until a key is pressed, and 5 seconds at least:
		// k, the cursor up on the only row, then the next key's own
		// refusal is to be seen.
		c.send("k")
		c.waitUntil(wait, "the flash is gone", func(lines []string) bool { return !lineWith(lines, "this key is read-only") })
		if row(c.scr.lines(), "hidden") == "" {
			t.Fatalf("after %s the table of secrets is gone:\n%s", key, c.scr)
		}
		noSecret(t, &c.term, "after "+key)
	}

	noSecret(t, &c.term, "at the end")

	for _, action := range []string{"yaml", "describe", "secret-data"} {
		eventually(t, wait, "kosh's log has the refused "+action, func() bool {
			return len(logLines(t, "refused", "key", k.name, "action", action, "object", "secrets/default/hidden")) > 0
		}, func() string { return "kosh's log:\n" + koshLog(t) })
	}
}

func TestUnknownKeyIsRefused(t *testing.T) {
	inCluster(t)
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	stranger := key{signer: signer, name: "stranger"}
	if c, err := dialSSH(t, stranger, 120, 40); err == nil {
		c.client.Close()
		t.Fatal("a key that is not in the list was accepted")
	} else if !strings.Contains(err.Error(), "unable to authenticate") {
		t.Fatalf("a key that is not in the list: %v, want a failed handshake", err)
	}
}

// runSSH runs the real ssh without a terminal and returns what it wrote
// on standard error and its exit status.
func runSSH(t *testing.T, cmd *exec.Cmd) (string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.String() + stderr.String(), 0
	case errors.As(err, &exit):
		return stdout.String() + stderr.String(), exit.ExitCode()
	default:
		t.Fatal(err)
		return "", -1
	}
}

func TestNoCommandNoShell(t *testing.T) {
	inCluster(t)
	k := fullKey(t)
	out, status := runSSH(t, sshCommand(t, k, []string{"-T"}, "ls"))
	if status != 1 || !strings.Contains(out, "kosh runs no commands") {
		t.Errorf("ssh -T kosh ls: status %d, output:\n%s\nwant status 1 and kosh runs no commands", status, out)
	}
	out, status = runSSH(t, sshCommand(t, k, []string{"-T"}))
	if status != 1 || !strings.Contains(out, "kosh needs a terminal") {
		t.Errorf("ssh -T kosh: status %d, output:\n%s\nwant status 1 and kosh needs a terminal", status, out)
	}
}

// headerLines returns the lines above the table's top border.
func headerLines(lines []string) []string {
	for i, l := range lines {
		if strings.HasPrefix(l, "┌") {
			return lines[:i]
		}
	}
	return lines
}

// cutHeader returns a line of the header that ends in "…", or "".
func cutHeader(lines []string) string {
	for _, l := range headerLines(lines) {
		if strings.HasSuffix(strings.TrimRight(l, " "), "…") {
			return l
		}
	}
	return ""
}

// The header shows only the columns of hints that fit whole, at 80
// columns as at 120.
func TestHeaderAt80(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 80, 24)
	c.waitFor("Pods(default)")
	c.fills(80, 24)
	if l := cutHeader(c.scr.lines()); l != "" {
		t.Errorf("at 80x24 a line of the header is cut: %q\n%s", l, c.scr)
	}
	c.resize(120, 40)
	c.fills(120, 40)
	if l := cutHeader(c.scr.lines()); l != "" {
		t.Errorf("at 120x40 a line of the header is cut: %q\n%s", l, c.scr)
	}
}

// servedAt is when kosh logged that it serves.
func servedAt(t *testing.T) time.Time {
	t.Helper()
	lines := logLines(t, "kosh is serving")
	if len(lines) == 0 {
		t.Fatalf("kosh's log has no line kosh is serving:\n%s", koshLog(t))
	}
	m := regexp.MustCompile(`^time=(\S+)`).FindStringSubmatch(lines[len(lines)-1])
	if m == nil {
		t.Fatalf("no time in %q", lines[len(lines)-1])
	}
	at, err := time.Parse(time.RFC3339Nano, m[1])
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// The kubelet's probes, connections that send no byte, are not in kosh's
// log at info level.
func TestProbesAreNotInTheLog(t *testing.T) {
	inCluster(t)
	at := servedAt(t)
	if d := 31*time.Second - time.Since(at); d > 0 {
		time.Sleep(d) // three probes of each kind at least
	}
	if lines := logLines(t, "connection closed before authenticating", "reason", "EOF"); len(lines) > 0 {
		t.Errorf("kosh has served %v and its log has %d probes, as: %s",
			time.Since(at).Round(time.Second), len(lines), lines[0])
	}
}

// A namespace given at the prompt must be a namespace's name: a path
// that would make the API server proxy a request is refused, and nothing
// of what such a request would answer reaches the terminal.
func TestNamespaceMustBeAName(t *testing.T) {
	inCluster(t)
	c := connect(t, readOnlyKey(t), 120, 40)
	c.waitFor("Pods(default)")
	// gitVersion is in what the API server's /version answers.
	c.scr.watch("gitVersion")
	t.Cleanup(func() {
		if seen := c.scr.seen(); len(seen) > 0 {
			t.Errorf("the API server's /version reached the terminal: %s", strings.Join(seen, "\n"))
		}
	})
	c.send(":leases ../../../../api/v1/namespaces/default/services/kubernetes:https/proxy/version" + keyEnter)
	c.waitFor("not a namespace")
	// Nothing is to be waited for: what would open would be there by now.
	time.Sleep(2 * time.Second)
	if lines := c.scr.lines(); !lineWith(lines, "Pods(default)") || lineWith(lines, "Leases(") {
		t.Errorf("the table shown is not the pods' any more:\n%s", c.scr)
	}
}

// logStreams is how many requests for a pod's log the API server is
// serving now, by its gauge of long-running requests.
func logStreams(t *testing.T) int {
	t.Helper()
	metrics := kubectl(t, "get", "--raw", "/metrics")
	n, found := 0, false
	sc := bufio.NewScanner(strings.NewReader(metrics))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if !strings.HasPrefix(l, "apiserver_longrunning_requests{") {
			continue
		}
		found = true
		if !strings.Contains(l, `resource="pods"`) || !strings.Contains(l, `subresource="log"`) {
			continue
		}
		f := strings.Fields(l)
		v, err := strconv.ParseFloat(f[len(f)-1], 64)
		if err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		n += int(v)
	}
	if !found {
		t.Fatal("the API server has no gauge apiserver_longrunning_requests")
	}
	return n
}

// A client that vanishes, killed instead of quitting, ends its session,
// and the log it was following is no longer read.
func TestVanishedClientEndsItsSession(t *testing.T) {
	inCluster(t)
	k := fullKey(t)
	eventually(t, wait, "no pod's log is being read", func() bool { return logStreams(t) == 0 },
		func() string { return fmt.Sprintf("%d logs are read", logStreams(t)) })
	before := len(logLines(t, "session start", "key", k.name))

	p := startPTY(t, sshCommand(t, k, []string{"-t"}), 120, 40)
	p.waitRow("marker", "Running")
	p.send("l")
	p.waitFor("kosh-e2e-marker")
	eventually(t, wait, "the API server serves the marker's log", func() bool { return logStreams(t) > 0 },
		func() string { return "the screen:\n" + p.scr.String() })
	t.Logf("with the log open, the API server serves %d log", logStreams(t))

	var start string
	eventually(t, wait, "kosh's log has the session's start", func() bool {
		lines := logLines(t, "session start", "key", k.name)
		if len(lines) <= before {
			return false
		}
		start = lines[len(lines)-1]
		return true
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	remote := regexp.MustCompile(`remote=(\S+)`).FindStringSubmatch(start)
	if remote == nil {
		t.Fatalf("the session's start names no remote: %s", start)
	}

	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	var end []string
	eventually(t, wait, "kosh's log has the end of the session of "+remote[1], func() bool {
		end = logLines(t, "session end", "remote", remote[1])
		return len(end) > 0
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	t.Logf("%v after the kill: %s", time.Since(killed).Round(100*time.Millisecond), end[0])
	eventually(t, wait, "the marker's log is no longer read", func() bool { return logStreams(t) == 0 },
		func() string { return fmt.Sprintf("%d logs are read", logStreams(t)) })
	t.Logf("%v after the kill, the API server serves no pod's log", time.Since(killed).Round(100*time.Millisecond))
}

// irohSSH runs the real iroh-ssh to kosh's endpoint ID as k, on a
// terminal of 120 by 40, with a home of its own.
func irohSSH(t *testing.T, k key) *ptySession {
	t.Helper()
	if cfg.noNetwork {
		t.Skip("KOSH_E2E_NO_NETWORK=1: iroh-ssh reaches kosh through number0's DNS and a relay")
	}
	return startPTY(t, irohSSHCommand(t, k), 120, 40)
}

// irohSSHCommand is the real iroh-ssh to kosh's endpoint ID as k, with
// kosh's host key checked: iroh-ssh hands ssh the endpoint ID as the
// host's name, so a known_hosts line of the endpoint ID and the host key
// is what ssh checks the key against. Its home is a temporary directory.
func irohSSHCommand(t *testing.T, k key) *exec.Cmd {
	t.Helper()
	known := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(known, []byte(knownHostsLine(cfg.endpoint, hostKey(t))+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(cfg.irohSSH, "-i", k.file,
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+known, "-o", "GlobalKnownHostsFile=/dev/null",
		"-o", "IdentitiesOnly=yes", "kosh@"+cfg.endpoint)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	return cmd
}

// overIroh waits for the screen of an iroh-ssh session, through the
// relays: 60 seconds, and kosh's log beside the screen when it fails.
func overIroh(t *testing.T, p *ptySession, what string, cond func(lines []string) bool) {
	t.Helper()
	eventually(t, 60*time.Second, "iroh-ssh: "+what, func() bool { return cond(p.scr.lines()) },
		func() string { return "the screen:\n" + p.scr.String() + "\n\nkosh's log:\n" + koshLog(t) })
}

func TestIrohSSH(t *testing.T) {
	inCluster(t)
	p := irohSSH(t, fullKey(t))
	overIroh(t, p, "the pods, and marker", func(lines []string) bool {
		return lineWith(lines, "Pods(default)") && row(lines, "marker") != ""
	})
	p.send(":q" + keyEnter)
	if status := p.exitedWith(wait); status != 0 {
		t.Errorf("iroh-ssh exited with status %d after :q; the screen:\n%s", status, p.scr)
	}
}

func TestIrohSSHReadOnly(t *testing.T) {
	inCluster(t)
	k := readOnlyKey(t)
	p := irohSSH(t, k)
	overIroh(t, p, "the read-only key in the header", func(lines []string) bool {
		return lineWith(lines, "Pods(default)") && lineWith(lines, "Key:", k.name+" [read-only]")
	})
	p.send(":q" + keyEnter)
	if status := p.exitedWith(wait); status != 0 {
		t.Errorf("iroh-ssh exited with status %d after :q; the screen:\n%s", status, p.scr)
	}
}

// sshPTY runs the real ssh to kosh over the forwarded port as k, on a
// pseudo-terminal of w by h, as a user runs it.
func sshPTY(t *testing.T, k key, w, h int) *ptySession {
	t.Helper()
	return startPTY(t, sshCommand(t, k, []string{"-t"}), w, h)
}

// resize changes the pseudo-terminal's size, as a terminal's window
// does: the program on it is told by SIGWINCH.
func (p *ptySession) resize(w, h int) {
	p.t.Helper()
	p.scr.resize(w, h)
	if err := pty.Setsize(p.ptmx, &pty.Winsize{Rows: uint16(h), Cols: uint16(w)}); err != nil {
		p.t.Fatal(err)
	}
}

// The keys of a shell's dialog and of a shell, as a terminal sends them.
const (
	keyUp    = "\x1b[A"
	keyDown  = "\x1b[B"
	keyLeft  = "\x1b[D"
	keyCtrlC = "\x03"
	keyCtrlU = "\x15" // in a text field: everything before the cursor deleted
)

// shellDefaultEnd is how the dialog's Command field shows the default
// command: scrolled to its end, where the cursor is.
const shellDefaultEnd = `exec bash || exec sh"`

// shellPrompt is the prompt of busybox's sh as root, without its blank.
const shellPrompt = "/ #"

// dialogLines returns the lines inside the dialog's box, without its
// borders and the blanks at their ends, or nil when no dialog is drawn.
func dialogLines(lines []string) []string {
	for i, l := range lines {
		r := []rune(l)
		left := slices.Index(r, '╭')
		right := slices.Index(r, '╮')
		if left < 0 || right <= left {
			continue
		}
		var inside []string
		for _, l := range lines[i+1:] {
			r := []rune(l)
			if len(r) <= right || r[left] == '╰' {
				break
			}
			inside = append(inside, strings.TrimSpace(string(r[left+1:right])))
		}
		return inside
	}
	return nil
}

// dialogText is the dialog's text, its lines joined by one space, every
// run of blanks one space: a reason wrapped onto several lines reads as
// one.
func dialogText(lines []string) string {
	return strings.Join(strings.Fields(strings.Join(dialogLines(lines), " ")), " ")
}

// dialogField returns the value of the dialog's field label as drawn, and
// whether the field has the focus; "" when there is no such field.
func dialogField(lines []string, label string) (value string, focused bool) {
	for _, l := range dialogLines(lines) {
		focused := strings.HasPrefix(l, "▸ ")
		l = strings.TrimSpace(strings.TrimPrefix(l, "▸ "))
		if rest, ok := strings.CutPrefix(l, label+"  "); ok {
			return strings.TrimSpace(rest), focused
		}
	}
	return "", false
}

// shellDialog presses s on the row under the cursor and waits for the
// shell's dialog for pod (namespace/name) with container chosen, as the
// field shows it, and the default command, in the field that has the
// focus.
func (c *term) shellDialog(pod, container string) {
	c.t.Helper()
	c.send("s")
	c.waitUntil(wait, fmt.Sprintf("the shell's dialog for %s, container %s, the default command", pod, container), func(lines []string) bool {
		got, _ := dialogField(lines, "Container")
		cmd, focused := dialogField(lines, "Command")
		return lineWith(dialogLines(lines), "A shell in pod "+pod) && got == container &&
			focused && strings.HasSuffix(cmd, shellDefaultEnd)
	})
}

// shellLine is the line kosh writes when a shell starts.
func shellLine(pod, container string) string {
	return fmt.Sprintf("kosh: a shell in pod %s, container %s. It ends with exit or ctrl-d.", pod, container)
}

// lastLine returns the index of the last line that is not blank, or -1.
func lastLine(lines []string) int {
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			return i
		}
	}
	return -1
}

// prompted reports whether the last line that is not blank is the
// shell's prompt alone, after line from (a command's end).
func prompted(lines []string, from int) bool {
	i := lastLine(lines)
	return i > from && strings.TrimSpace(lines[i]) == shellPrompt
}

// inShell waits for kosh's line for pod and container, and the shell's
// prompt after it.
func (c *term) inShell(pod, container string) {
	c.t.Helper()
	want := shellLine(pod, container)
	c.waitUntil(wait, "kosh's line "+want+", then the prompt", func(lines []string) bool {
		for i, l := range lines {
			if strings.TrimSpace(l) == want {
				return prompted(lines, i)
			}
		}
		return false
	})
}

// shellRun types command into the shell, and waits for a line that is
// want alone, and the prompt after it.
func (c *term) shellRun(command, want string) {
	c.t.Helper()
	c.send(command + keyEnter)
	c.waitUntil(wait, fmt.Sprintf("%s shows %s, then the prompt", command, want), func(lines []string) bool {
		for i := len(lines) - 1; i >= 0; i-- {
			if strings.TrimRight(lines[i], " ") == want {
				return prompted(lines, i)
			}
		}
		return false
	})
}

// sizeFollows waits until stty size in the shell shows want. The new
// size reaches the container beside what is typed, not in order with it,
// as over kubectl exec -it: a command typed at once after a resize may
// still see the old size, and stty size is asked again until the
// deadline.
func (c *term) sizeFollows(want string) {
	c.t.Helper()
	deadline := time.Now().Add(wait)
	var got string
	for i := 1; ; i++ {
		mark := fmt.Sprintf("kosh-size-%d", i)
		c.send("echo " + mark + " $(stty size)" + keyEnter)
		c.waitUntil(wait, "the answer to "+mark+", then the prompt", func(lines []string) bool {
			for j := len(lines) - 1; j >= 0; j-- {
				if rest, ok := strings.CutPrefix(strings.TrimRight(lines[j], " "), mark+" "); ok {
					got = rest
					return prompted(lines, j)
				}
			}
			return false
		})
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("%s: stty size shows %s, want %s, %v after the resize; the screen:\n%s", c.what, got, want, wait, c.scr)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// openShell presses s on marker, the first row of the pods of default,
// and enter, and waits for the shell.
func (c *term) openShell() {
	c.t.Helper()
	c.waitRow("marker", "Running")
	c.shellDialog("default/marker", "marker")
	c.send(keyEnter)
	c.inShell("default/marker", "marker")
}

// backFromShell types exit, and waits for the pods table, whole, and
// marker in it.
func (c *term) backFromShell(w, h int) {
	c.t.Helper()
	c.send("exit" + keyEnter)
	c.fills(w, h)
	c.waitRow("marker", "Running")
}

func TestShell(t *testing.T) {
	inCluster(t)
	p := sshPTY(t, fullKey(t), 120, 40)
	p.waitRow("marker", "Running")
	p.waitUntil(wait, "the hint <s> Shell", func(lines []string) bool { return lineWith(headerLines(lines), "<s> Shell") })
	p.openShell()
	p.shellRun("echo kosh-$((6*7))", "kosh-42")
	p.shellRun("stty size", "40 120")
	p.resize(100, 30)
	p.sizeFollows("30 100")
	p.backFromShell(100, 30)

	// The table follows the cluster again.
	const pod = "shell-follows"
	kubectl(t, "--namespace", "default", "run", pod, "--image=registry.k8s.io/pause:3.10",
		"--image-pull-policy=IfNotPresent", "--restart=Never")
	t.Cleanup(func() { kubectl(t, "--namespace", "default", "delete", "pod", pod, "--wait=false") })
	p.waitRow(pod)
}

// A full-screen program in the shell gets the terminal as a terminal
// has it: its bare line feeds and its cursor's moves arrive untouched,
// and ctrl-c reaches what runs in the shell.
func TestShellFullScreen(t *testing.T) {
	inCluster(t)
	const w, h = 120, 40
	p := sshPTY(t, fullKey(t), w, h)
	p.openShell()

	// A bare line feed moves down and not back: a staircase.
	p.send(`stty -onlcr; printf 'kosh-ab\nkosh-cd\n'; stty onlcr` + keyEnter)
	p.waitUntil(wait, "kosh-cd under the end of kosh-ab, then the prompt", func(lines []string) bool {
		for i := 0; i+1 < len(lines); i++ {
			if strings.TrimRight(lines[i], " ") == "kosh-ab" && strings.TrimRight(lines[i+1], " ") == "       kosh-cd" {
				return prompted(lines, i+1)
			}
		}
		return false
	})

	const file, text = "/tmp/kosh-e2e.txt", "kosh-e2e-vi-line"
	// busybox's sh drops what is typed after the enter of a command
	// before the command runs, as it does over kubectl exec -it: each
	// key below is typed once what it is for is on the screen.
	p.send("rm -f " + file + "; vi " + file + keyEnter)
	p.waitUntil(wait, "vi, empty, with a ~ alone at the first column of every empty row and its status line last", func(lines []string) bool {
		return viScreen(lines, "", file)
	})
	p.send("i" + text + keyEsc)
	p.waitUntil(wait, "vi with the line typed, a ~ alone at the first column of every empty row and its status line last", func(lines []string) bool {
		return viScreen(lines, text, file)
	})
	p.send(":wq" + keyEnter)
	p.waitUntil(wait, "the prompt after vi", func(lines []string) bool { return prompted(lines, -1) })
	p.shellRun("clear; cat "+file, text)

	before := running(t, "marker", "sleep 100")
	p.send("sleep 100" + keyEnter)
	started(t, p.scr, "marker", "sleep 100", before)
	p.send(keyCtrlC)
	p.shellRun("echo $?", "130")
	p.backFromShell(w, h)
}

// viScreen reports whether lines are busybox vi's screen of a file of
// one line, first (empty for an empty file): the line on the first row,
// a ~ alone at the first column of every row after it but the last, and
// the status line, which names the file, on the last.
func viScreen(lines []string, first, file string) bool {
	h := len(lines)
	if h < 3 || !strings.Contains(lines[h-1], file) || strings.TrimRight(lines[0], " ") != first {
		return false
	}
	for _, l := range lines[1 : h-1] {
		if strings.TrimRight(l, " ") != "~" {
			return false
		}
	}
	return true
}

// running returns the PIDs of the processes of the pod's (first)
// container that run command, as ps shows them.
func running(t *testing.T, pod, command string) []string {
	t.Helper()
	var pids []string
	for _, l := range strings.Split(kubectl(t, "--namespace", "default", "exec", pod, "--", "ps", "-o", "pid,args"), "\n") {
		if pid, args, ok := strings.Cut(strings.TrimSpace(l), " "); ok && strings.TrimSpace(args) == command {
			pids = append(pids, pid)
		}
	}
	return pids
}

// started waits until command runs in the pod's container in a process
// that was not among before, and returns its PID.
func started(t *testing.T, scr *screen, pod, command string, before []string) string {
	t.Helper()
	pid := ""
	eventually(t, wait, command+" runs in "+pod, func() bool {
		for _, p := range running(t, pod, command) {
			if !slices.Contains(before, p) {
				pid = p
				return true
			}
		}
		return false
	}, func() string { return "the screen:\n" + scr.String() })
	return pid
}

// A command of the user's: its words reach the container as the field
// cuts them, the third word whole, spaces and all, and the fourth as $0.
func TestShellEditedCommand(t *testing.T) {
	inCluster(t)
	p := sshPTY(t, fullKey(t), 120, 40)
	p.waitRow("marker", "Running")
	p.shellDialog("default/marker", "marker")
	const command = `/bin/sh -c "echo edited-$0; exec sh" kosh-e2e`
	p.send(keyCtrlU, command)
	p.waitUntil(wait, "the Command field holds "+command, func(lines []string) bool {
		got, _ := dialogField(lines, "Command")
		return got == command
	})
	p.send(keyEnter)
	p.inShell("default/marker", "marker")
	p.waitUntil(wait, "edited-kosh-e2e, then the prompt", func(lines []string) bool {
		for i, l := range lines {
			if strings.TrimRight(l, " ") == "edited-kosh-e2e" {
				return prompted(lines, i)
			}
		}
		return false
	})
	p.backFromShell(120, 40)
}

// A command the container does not have: the dialog again, with the
// text as typed and the runtime's reason.
func TestShellCommandThatIsNotThere(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 120, 40)
	c.waitRow("marker", "Running")
	c.fills(120, 40)
	c.shellDialog("default/marker", "marker")
	const command = "/no/such/shell"
	c.send(keyCtrlU, command, keyEnter)
	c.waitUntil(wait, "the dialog again, with "+command+" and the reason", func(lines []string) bool {
		got, _ := dialogField(lines, "Command")
		return got == command && strings.Contains(dialogText(lines), "no such file or directory")
	})
	t.Logf("the dialog:\n%s", strings.Join(dialogLines(c.scr.lines()), "\n"))
	c.send(keyEsc)
	c.waitUntil(wait, "the dialog gone", func(lines []string) bool { return dialogLines(lines) == nil })
	c.fills(120, 40)
	c.waitRow("marker", "Running")
	c.waitFor("Pods(default)")
	// The screen is alive after a hand-over that returned at once: it
	// follows the terminal's size.
	c.resize(100, 30)
	c.fills(100, 30)
	c.waitRow("marker", "Running")
}

// kosh's own image has no shell at all: the default command's reason, in
// the dialog, at once.
func TestShellPodWithoutAShell(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 120, 40)
	c.waitFor("Pods(default)")
	c.send(":pods " + namespace + keyEnter)
	c.waitFor("Pods(" + namespace + ")")
	pod := ""
	c.waitUntil(wait, "kosh's pod, running", func(lines []string) bool {
		for _, l := range lines {
			cells := strings.Fields(strings.Trim(l, "│ "))
			if len(cells) > 2 && strings.HasPrefix(cells[0], release+"-") && strings.Contains(l, "Running") {
				pod = cells[0]
				return true
			}
		}
		return false
	})
	c.shellDialog(namespace+"/"+pod, "kosh")
	c.send(keyEnter)
	c.waitUntil(wait, "the dialog again, with the reason", func(lines []string) bool {
		got, _ := dialogField(lines, "Command")
		return strings.HasSuffix(got, shellDefaultEnd) && strings.Contains(dialogText(lines), "executable file not found")
	})
	t.Logf("the dialog:\n%s", strings.Join(dialogLines(c.scr.lines()), "\n"))
	c.send(keyEsc)
	c.waitUntil(wait, "the dialog gone", func(lines []string) bool { return dialogLines(lines) == nil })
	c.fills(120, 40)
	c.waitRow(pod, "Running")
	// The screen is alive after a hand-over that returned at once: it
	// follows the terminal's size.
	c.resize(100, 30)
	c.fills(100, 30)
	c.waitRow(pod, "Running")
}

func TestShellStatus(t *testing.T) {
	inCluster(t)
	p := sshPTY(t, fullKey(t), 120, 40)
	p.openShell()
	p.send("exit 3" + keyEnter)
	p.fills(120, 40)
	p.waitRow("marker", "Running")
	p.waitFor("the shell ended with status 3")
}

// From a pod's containers, the container is the row's, and the focus
// passes it by.
func TestShellFromTheContainersTable(t *testing.T) {
	inCluster(t)
	p := sshPTY(t, fullKey(t), 120, 40)
	p.waitRow("marker", "Running")
	p.send(keyEnter)
	p.waitFor("Containers(pods/default/marker)")
	p.waitRow("M1", "marker")
	p.shellDialog("default/marker", "marker")
	p.send(keyUp)
	p.waitUntil(wait, "the focus on Shell, past Container", func(lines []string) bool {
		got, focused := dialogField(lines, "Container")
		return strings.Contains(dialogText(lines), "[ Shell ]") && got == "marker" && !focused
	})
	p.send(keyDown)
	p.waitUntil(wait, "the focus in Command again", func(lines []string) bool {
		_, focused := dialogField(lines, "Command")
		return focused
	})
	p.send(keyEnter)
	p.inShell("default/marker", "marker")
	p.send("exit" + keyEnter)
	p.fills(120, 40)
	p.waitFor("Containers(pods/default/marker)")

	// Of pair, the container that is not the one the annotation names.
	p.send(keyEsc)
	p.waitFor("Pods(default)")
	p.send("/pair" + keyEnter)
	p.waitUntil(wait, "pair alone", func(lines []string) bool {
		return row(lines, "pair", "Running") != "" && row(lines, "marker") == ""
	})
	p.send(keyEnter)
	p.waitFor("Containers(pods/default/pair)")
	p.waitRow("M1", "first")
	p.waitRow("M2", "second")
	p.shellDialog("default/pair", "first")
	p.send(keyEnter)
	p.inShell("default/pair", "first")
	p.shellRun("cat /tmp/kosh-e2e-container", "kosh-e2e-in-first")
	p.send("exit" + keyEnter)
	p.waitFor("Containers(pods/default/pair)")
}

// A pod of two containers: the one the annotation names first, and the
// other chosen with the arrows.
func TestShellSeveralContainers(t *testing.T) {
	inCluster(t)
	p := sshPTY(t, fullKey(t), 120, 40)
	p.waitRow("pair", "Running")
	p.send("/pair" + keyEnter)
	p.waitUntil(wait, "pair alone", func(lines []string) bool {
		return row(lines, "pair", "Running") != "" && row(lines, "marker") == ""
	})
	p.shellDialog("default/pair", "‹ second ›")
	p.send(keyUp)
	p.waitUntil(wait, "the focus on Container", func(lines []string) bool {
		_, focused := dialogField(lines, "Container")
		return focused
	})
	p.send(keyLeft)
	p.waitUntil(wait, "the container first", func(lines []string) bool {
		got, _ := dialogField(lines, "Container")
		return got == "‹ first ›"
	})
	p.send(keyDown)
	p.waitUntil(wait, "the focus in Command again", func(lines []string) bool {
		_, focused := dialogField(lines, "Command")
		return focused
	})
	p.send(keyEnter)
	p.inShell("default/pair", "first")
	p.shellRun("cat /tmp/kosh-e2e-container", "kosh-e2e-in-first")
	p.send("exit" + keyEnter)
	p.fills(120, 40)
	p.waitRow("pair", "Running")

	// And the second, as the dialog chose it.
	p.shellDialog("default/pair", "‹ second ›")
	p.send(keyEnter)
	p.inShell("default/pair", "second")
	p.shellRun("cat /tmp/kosh-e2e-container", "kosh-e2e-in-second")
	p.send("exit" + keyEnter)
	p.fills(120, 40)
	p.waitRow("pair", "Running")
}

func TestShellOverIroh(t *testing.T) {
	inCluster(t)
	p := irohSSH(t, fullKey(t))
	overIroh(t, p, "the pods, and marker", func(lines []string) bool {
		return lineWith(lines, "Pods(default)") && row(lines, "marker", "Running") != ""
	})
	p.shellDialog("default/marker", "marker")
	p.send(keyEnter)
	p.inShell("default/marker", "marker")
	p.shellRun("echo kosh-$((6*7))", "kosh-42")
	p.backFromShell(120, 40)
	p.send(":q" + keyEnter)
	if status := p.exitedWith(wait); status != 0 {
		t.Errorf("iroh-ssh exited with status %d after :q; the screen:\n%s", status, p.scr)
	}
}

// A read-only key has no shell: no hint, and s does nothing. TestShell
// is the positive control: a full key sees <s> Shell.
func TestShellReadOnlyKey(t *testing.T) {
	inCluster(t)
	k := readOnlyKey(t)
	c := connect(t, k, 120, 40)
	c.waitUntil(wait, "the key's name with [read-only] in the header", func(lines []string) bool {
		return lineWith(lines, "Key:", k.name+" [read-only]")
	})
	c.waitRow("marker", "Running")
	c.fills(120, 40)
	before := c.scr.lines()
	if lineWith(headerLines(before), "<s> Shell") {
		t.Errorf("a read-only key is offered <s> Shell:\n%s", c.scr)
	}
	c.send("s")
	// Nothing is to be waited for: what s would open would be there by now.
	time.Sleep(2 * time.Second)
	after := c.scr.lines()
	if dialogLines(after) != nil || lineWith(after, "A shell in pod") {
		t.Errorf("s opened something for a read-only key:\n%s", c.scr)
	}
	// The frame is as it was: the header, the table's title, the crumbs
	// and the flash. (The table's ages may have moved on.)
	same := func(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }
	h := len(after)
	if !same(headerLines(after), headerLines(before)) || !same(after[h-2:], before[h-2:]) ||
		!same(after[len(headerLines(after)):][:1], before[len(headerLines(before)):][:1]) {
		t.Errorf("the frame changed after s.\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), c.scr)
	}
	if lines := logLines(t, "action", "key", k.name, "action", "exec"); len(lines) > 0 {
		t.Errorf("kosh's log has an exec of the read-only key: %s", lines[0])
	}
}

// The two lines of a shell in kosh's log: who, in what, with which
// command; and how it ended. Nothing of what was typed or printed.
func TestShellIsLogged(t *testing.T) {
	inCluster(t)
	k := fullKey(t)
	attrs := []string{"key", k.name, "fingerprint", ssh.FingerprintSHA256(k.signer.PublicKey()),
		"action", "exec", "object", "pods/default/marker", "container", "marker"}
	start := append(slices.Clone(attrs), "phase", "start",
		"command", `"sh" "-c" "command -v bash >/dev/null && exec bash || exec sh"`)
	end := append(slices.Clone(attrs), "phase", "end", "started", "true", "status", "0")
	starts, ends := len(logLines(t, "action", start...)), len(logLines(t, "action", end...))

	const secret = "kosh-secret-marker"
	p := sshPTY(t, k, 120, 40)
	p.openShell()
	p.shellRun("echo "+secret, secret)
	p.backFromShell(120, 40)

	var got []string
	eventually(t, wait, "kosh's log has the shell's start and end", func() bool {
		s, e := logLines(t, "action", start...), logLines(t, "action", end...)
		if len(s) <= starts || len(e) <= ends {
			return false
		}
		got = []string{s[len(s)-1], e[len(e)-1]}
		return true
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	t.Logf("kosh's log:\n%s\n%s", got[0], got[1])
	if !regexp.MustCompile(` duration=\d[^ ]*s `).MatchString(got[1] + " ") {
		t.Errorf("the end line has no duration: %s", got[1])
	}
	if log := koshLog(t); strings.Contains(log, secret) {
		t.Errorf("kosh's log has what was typed in the shell, %s:\n%s", secret, log)
	}
}

// The client killed while its shell is open: the session ends, and the
// shell's line of the end with it; the key opens a session again.
func TestShellSurvivesNothing(t *testing.T) {
	inCluster(t)
	k := fullKey(t)
	sessions := len(logLines(t, "session start", "key", k.name))
	end := []string{"key", k.name, "action", "exec", "object", "pods/default/marker", "phase", "end"}
	shells := len(logLines(t, "action", end...))

	p := sshPTY(t, k, 120, 40)
	p.openShell()
	var start string
	eventually(t, wait, "kosh's log has the session's start", func() bool {
		lines := logLines(t, "session start", "key", k.name)
		if len(lines) <= sessions {
			return false
		}
		start = lines[len(lines)-1]
		return true
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	remote := regexp.MustCompile(`remote=(\S+)`).FindStringSubmatch(start)
	if remote == nil {
		t.Fatalf("the session's start names no remote: %s", start)
	}

	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	killed := time.Now()
	var ended []string
	eventually(t, 15*time.Second, "kosh's log has the end of the session of "+remote[1]+" and of its shell", func() bool {
		ended = logLines(t, "action", end...)
		return len(logLines(t, "session end", "remote", remote[1])) > 0 && len(ended) > shells
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	t.Logf("%v after the kill: %s", time.Since(killed).Round(100*time.Millisecond), logLines(t, "session end", "remote", remote[1])[0])
	shell := ended[len(ended)-1]
	t.Logf("and: %s", shell)
	// The stream was cut, not the shell ended: the spec's §7.
	if !hasAttr(shell, "ended", "the session ended") {
		t.Errorf("the shell's end line does not say ended=%q: %s", "the session ended", shell)
	}

	c := connect(t, k, 120, 40)
	c.waitRow("marker", "Running")
	c.fills(120, 40)

	// A record, not a requirement: how often a screen that the UI
	// library left dead after a command had to be repaired, in kosh's
	// log of every shell so far.
	repaired := logLines(t, "the program's screen was repaired after a command")
	t.Logf("kosh's log has %d lines of a screen repaired after a command", len(repaired))
	for _, l := range repaired {
		t.Log(l)
	}
}

// setKeys installs the chart again with the key list of lines, keeping
// every other value.
func setKeys(t *testing.T, lines ...string) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := run("helm", "upgrade", release, chart, "--namespace", namespace, "--reuse-values",
		"--set-file", "authorizedKeys="+file); err != nil {
		t.Fatal(err)
	}
}

// bindingRole returns the role of the ClusterRoleBinding name, or "" when
// there is none.
func bindingRole(t *testing.T, name string) string {
	t.Helper()
	return strings.TrimSpace(kubectl(t, "get", "clusterrolebinding", name, "--ignore-not-found", "-o", "jsonpath={.roleRef.name}"))
}

// koshMayScale is kubectl auth can-i for kosh's ServiceAccount and a
// scale in default: "yes" or "no".
func koshMayScale(t *testing.T) string {
	t.Helper()
	out, _ := run("kubectl", "auth", "can-i", "update", "deployments.apps/scale", "--namespace", "default",
		"--as", "system:serviceaccount:"+namespace+":"+release)
	return strings.TrimSpace(out)
}

// setRole installs the chart again with rbac.clusterRole, keeping every
// other value.
func setRole(t *testing.T, role string) {
	t.Helper()
	if _, err := run("helm", "upgrade", release, chart, "--namespace", namespace, "--reuse-values",
		"--set", "rbac.clusterRole="+role); err != nil {
		t.Fatal(err)
	}
}

// The ClusterRole can be changed with helm upgrade, and a full key then
// meets the API's refusal, readably.
func TestRoleCanBeChanged(t *testing.T) {
	inCluster(t)
	setRole(t, "view")
	t.Cleanup(func() {
		setRole(t, "cluster-admin")
		eventually(t, wait, "cluster-admin is kosh's again", func() bool {
			return bindingRole(t, release+"-cluster-admin") == "cluster-admin" && bindingRole(t, release+"-view") == "" &&
				koshMayScale(t) == "yes"
		}, func() string { return "can-i: " + koshMayScale(t) })
	})
	if got := bindingRole(t, release+"-view"); got != "view" {
		t.Errorf("the binding %s-view binds %q, want view", release, got)
	}
	if got := bindingRole(t, release+"-cluster-admin"); got != "" {
		t.Errorf("the binding %s-cluster-admin is still there, binding %s", release, got)
	}
	eventually(t, wait, "kosh may no longer scale", func() bool { return koshMayScale(t) == "no" },
		func() string { return "can-i: " + koshMayScale(t) })

	before := replicas(t)
	c := connect(t, fullKey(t), 120, 40)
	c.waitFor("Pods(default)")
	c.send(":deploy" + keyEnter)
	c.waitRow("web")
	c.send("s")
	c.waitFor("Scale deployments/default/web?", "Replicas")
	c.send(scaleTo3...)
	c.send(scaleConfirm...)
	c.waitUntil(wait, "the API's refusal, whole", func(lines []string) bool {
		var trimmed []string
		for _, l := range lines {
			trimmed = append(trimmed, strings.TrimSpace(l))
		}
		all := strings.Join(trimmed, " ")
		return strings.Contains(all, "forbidden") && strings.Contains(all, "cannot update")
	})
	for _, l := range c.scr.lines() {
		if strings.Contains(l, "forbidden") && strings.HasSuffix(strings.TrimRight(l, " "), "…") {
			t.Errorf("the refusal is cut: %q\n%s", l, c.scr)
		}
	}
	if after := replicas(t); after != before {
		t.Errorf("the replicas of web went from %s to %s under the role view", before, after)
	}
}

// It takes the read-only key out of the list, and puts it back.
func TestKeyRemovedEndsTheSession(t *testing.T) {
	inCluster(t)
	full, ro := fullKey(t), readOnlyKey(t)
	f := connect(t, full, 120, 40)
	f.waitFor("Pods(default)", full.name)
	r := connect(t, ro, 120, 40)
	r.waitFor("Pods(default)", ro.name+" [read-only]")

	setKeys(t, full.line)
	t.Cleanup(func() {
		// Back as it was, and waited for, so that a test after this one
		// does not meet the kubelet's delay.
		setKeys(t, full.line, "read-only "+ro.line)
		var err error
		eventually(t, 150*time.Second, "the read-only key is accepted again", func() bool {
			var c *goSession
			if c, err = dialSSH(t, ro, 120, 40); err == nil {
				_ = c.client.Close()
				return true
			}
			return false
		}, func() string { return fmt.Sprintf("the last attempt: %v", err) })
	})
	changed := time.Now()

	// The kubelet updates a mounted ConfigMap in its own time.
	err := r.ended(120 * time.Second)
	t.Logf("the read-only key's session ended %v after the list changed", time.Since(changed).Round(100*time.Millisecond))
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 1 {
		t.Errorf("the read-only key's session ended with %v, want status 1", err)
	}
	said := r.scr.String() + "\n" + r.stderr.String()
	if !strings.Contains(said, "this key is no longer accepted") {
		t.Errorf("the session was not told that its key is no longer accepted:\n%s", said)
	}

	if !f.alive() {
		t.Fatal("the full key's session ended too")
	}
	f.send(":ns" + keyEnter)
	f.waitFor("Namespaces(all)")
	f.waitRow("kosh")

	if c, err := dialSSH(t, ro, 120, 40); err == nil {
		_ = c.client.Close()
		t.Error("the read-only key is still accepted for a new connection")
	} else if !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("a new connection of the read-only key: %v, want a failed handshake", err)
	}
}

// newKey makes a key of the test's own, named name, with its files in a
// temporary directory.
func newKey(t *testing.T, name string) key {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, name)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(file, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + name
	if err := os.WriteFile(file+".pub", []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadKey(t, file)
}

// accepted reports whether k opens a session.
func accepted(t *testing.T, k key) bool {
	t.Helper()
	c, err := dialSSH(t, k, 120, 40)
	if err != nil {
		return false
	}
	_ = c.client.Close()
	return true
}

// A shell ends with its session when its key leaves the list. What it
// ran in the container may go on (the spec's §7): the test says whether
// it did, and does not require either.
func TestShellEndsWithItsKey(t *testing.T) {
	inCluster(t)
	full, ro := fullKey(t), readOnlyKey(t)
	k := newKey(t, "carol@kosh-e2e")
	setKeys(t, full.line, "read-only "+ro.line, k.line)
	t.Cleanup(func() {
		// The list as it was, and waited for.
		setKeys(t, full.line, "read-only "+ro.line)
		eventually(t, 150*time.Second, "the key of the test is refused again", func() bool { return !accepted(t, k) },
			func() string { return "it is still accepted" })
	})
	// The kubelet updates a mounted ConfigMap in its own time.
	eventually(t, 150*time.Second, "the key of the test is accepted", func() bool { return accepted(t, k) },
		func() string { return "it is not" })

	// A session of another key, which the removal leaves alone.
	f := connect(t, full, 120, 40)
	f.waitFor("Pods(default)", full.name)
	p := sshPTY(t, k, 120, 40)
	p.openShell()
	const orphan = "sleep 300"
	before := running(t, "marker", orphan)
	p.send(orphan + keyEnter)
	pid := started(t, p.scr, "marker", orphan, before)
	end := []string{"key", k.name, "action", "exec", "object", "pods/default/marker", "phase", "end"}
	shells := len(logLines(t, "action", end...))

	setKeys(t, full.line, "read-only "+ro.line)
	changed := time.Now()
	status := p.exitedWith(120 * time.Second)
	t.Logf("ssh ended %v after the list changed, with status %d", time.Since(changed).Round(100*time.Millisecond), status)
	if said := p.scr.String() + "\n" + ansi.Strip(p.scr.raw()); !strings.Contains(said, "this key is no longer accepted") {
		t.Errorf("the client was not told that its key is no longer accepted:\n%s", p.scr)
	}
	var ended []string
	eventually(t, wait, "kosh's log has the end of the shell", func() bool {
		ended = logLines(t, "action", end...)
		return len(ended) > shells
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	shell := ended[len(ended)-1]
	t.Logf("kosh's log: %s", shell)
	if !hasAttr(shell, "ended", "the session ended") {
		t.Errorf("the shell's end line does not say ended=%q: %s", "the session ended", shell)
	}
	// A record of the limit the spec states, not a requirement.
	t.Logf("%s, PID %s in marker, after its session ended: still running %v", orphan, pid, slices.Contains(running(t, "marker", orphan), pid))

	if !f.alive() {
		t.Fatalf("the full key's session ended too, with %v; the screen:\n%s", <-f.waited, f.scr)
	}
	f.send(":ns" + keyEnter)
	f.waitFor("Namespaces(all)")
	f.waitRow("kosh")
}

// The last test: it restarts kosh. A stop tells every session first,
// over TCP and over iroh, and logs why each ended.
func TestStopTellsTheSessions(t *testing.T) {
	inCluster(t)
	pods := strings.Fields(kubectl(t, "--namespace", namespace, "get", "pods", "-o", "jsonpath={.items[*].metadata.name}"))
	if len(pods) != 1 {
		t.Fatalf("pods of kosh: %q, want one", pods)
	}
	// The log of the pod that stops, followed from before the stop: the
	// pod is deleted with it.
	var podLog syncBuffer
	follow := exec.Command("kubectl", "--namespace", namespace, "logs", "--follow", pods[0])
	follow.Stdout, follow.Stderr = &podLog, &podLog
	if err := follow.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = follow.Process.Kill()
		_ = follow.Wait()
	})

	k := fullKey(t)
	c := connect(t, k, 120, 40)
	c.waitFor("Pods(default)")
	c.scr.watch(stopping)
	var p *ptySession
	if !cfg.noNetwork {
		p = irohSSH(t, k)
		overIroh(t, p, "the pods", func(lines []string) bool { return lineWith(lines, "Pods(default)") })
		p.scr.watch(stopping)
	}

	kubectl(t, "--namespace", namespace, "rollout", "restart", "deployment/"+release)
	t.Cleanup(func() {
		if _, err := run("kubectl", "--namespace", namespace, "rollout", "status", "deployment/"+release, "--timeout=180s"); err != nil {
			t.Errorf("kosh is not back: %v", err)
		}
	})

	told := func(scr *screen) bool { return len(scr.seen()) > 0 }
	eventually(t, wait, "the session over TCP is told", func() bool { return told(c.scr) },
		func() string { return "the screen:\n" + c.scr.String() })
	_ = c.ended(wait)
	transports := []string{"tcp"}
	if p != nil {
		eventually(t, wait, "the session over iroh is told", func() bool { return told(p.scr) },
			func() string { return "the screen:\n" + p.scr.String() })
		p.exitedWith(wait)
		transports = append(transports, "iroh")
	}
	for _, tr := range transports {
		eventually(t, wait, "the log of the old pod has the end of the session over "+tr, func() bool {
			for _, l := range strings.Split(podLog.String(), "\n") {
				if hasAttr(l, "msg", "session end") && hasAttrs(l, "transport", tr, "key", k.name, "reason", stopping) {
					return true
				}
			}
			return false
		}, func() string { return "the log of the old pod:\n" + podLog.String() })
	}
}

// stopping is what a session is told when kosh stops.
const stopping = "kosh is shutting down"

// iroh-ssh finds kosh, which runs as a process on this machine with the
// relays off, by mDNS alone: scripts/test-mdns.sh.
func TestIrohSSHFindsKoshByMDNS(t *testing.T) {
	if !cfg.mdns {
		t.Skip("KOSH_E2E_MDNS is not 1: scripts/test-mdns.sh runs this test")
	}
	need(t, "KOSH_E2E_FULL_KEY", "KOSH_E2E_HOST_KEY", "KOSH_E2E_ENDPOINT", "KOSH_E2E_IROH_SSH", "KOSH_E2E_LOG")
	k := fullKey(t)
	p := startPTY(t, irohSSHCommand(t, k), 120, 40)
	overIroh(t, p, "the pods, found by mDNS", func(lines []string) bool {
		return lineWith(lines, "Pods(default)") && lineWith(lines, "Key:", k.name)
	})
	p.send(":q" + keyEnter)
	if status := p.exitedWith(wait); status != 0 {
		t.Errorf("iroh-ssh exited with status %d after :q; the screen:\n%s", status, p.scr)
	}
}
