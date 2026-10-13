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
// cluster and one kosh: TestKeyRemovedEndsTheSession takes the read-only
// key out of the list and comes last.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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
	c.send("s")
	c.waitFor("Scale deployments/default/web?", "Replicas")
	c.send(keyBackspace, "3")
	c.waitUntil(wait, "the field holds 3", func(lines []string) bool {
		return row(lines, "Replicas", "3") != "" || lineWith(lines, "Replicas", " 3 ")
	})
	c.send(keyEnter) // from the field to Cancel
	c.waitFor("[ Cancel ]")
	c.send(keyRight)
	c.waitFor("[ Scale ]")
	c.send(keyEnter)

	eventually(t, 10*time.Second, "the deployment web has 3 replicas", func() bool { return replicas(t) == "3" },
		func() string { return "replicas: " + replicas(t) + "\nthe screen:\n" + c.scr.String() })
	eventually(t, wait, "kosh's log has the action", func() bool {
		return len(logLines(t, "action", "key", k.name, "action", "scale", "object", "deployments/default/web")) > 0
	}, func() string { return "kosh's log:\n" + koshLog(t) })
}

// podExists reports whether the pod is there and not being deleted.
func podExists(t *testing.T, name string) bool {
	t.Helper()
	out, err := run("kubectl", "--namespace", "default", "get", "pod", name, "--ignore-not-found",
		"-o", "jsonpath={.metadata.name} {.metadata.deletionTimestamp}")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out) == name
}

func TestSSHKillAsksFirst(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 120, 40)
	c.waitFor("Pods(default)")
	c.send(":deploy" + keyEnter)
	c.waitRow("web")
	c.send(keyEnter)
	pod := c.firstPodOfWeb()
	question := "Kill pods/default/" + pod + "?"

	// The focus is on Cancel: enter at once kills nothing.
	c.send(keyCtrlK)
	c.waitFor(question, "[ Cancel ]")
	c.send(keyEnter)
	c.waitUntil(wait, "the dialog is gone", func(lines []string) bool { return !lineWith(lines, question) })
	if !podExists(t, pod) {
		t.Fatalf("the pod %s is gone after Cancel", pod)
	}

	c.send(keyCtrlK)
	c.waitFor(question, "[ Cancel ]")
	c.send(keyRight)
	c.waitFor("[ Kill ]")
	c.send(keyEnter)
	eventually(t, wait, "the pod "+pod+" is deleted", func() bool { return !podExists(t, pod) },
		func() string { return "the screen:\n" + c.scr.String() })
}

// secretValue is the value of the Secret hidden.
const secretValue = "kosh-e2e-secret-value"

func TestReadOnlyKey(t *testing.T) {
	inCluster(t)
	k := readOnlyKey(t)
	c := connect(t, k, 120, 40)
	c.waitUntil(wait, "the key's name with [read-only] in the header", func(lines []string) bool {
		return lineWith(lines, "Key:", k.name+" [read-only]")
	})
	// noSecret checks, at every step, that the Secret's value is not on
	// the screen, and was never sent to it.
	noSecret := func(when string) {
		t.Helper()
		if strings.Contains(c.scr.String(), secretValue) {
			t.Fatalf("%s: the screen shows the Secret's value:\n%s", when, c.scr)
		}
		if strings.Contains(c.scr.raw(), secretValue) {
			t.Fatalf("%s: kosh sent the Secret's value to a read-only key", when)
		}
	}

	c.send(":deploy" + keyEnter)
	c.waitRow("web")
	if lineWith(c.scr.lines(), "<s>", "Scale") {
		t.Errorf("a read-only key is offered <s> Scale:\n%s", c.scr)
	}
	before := replicas(t)
	c.send("s")
	// Nothing is to be waited for: what s would open would be there by
	// now.
	time.Sleep(2 * time.Second)
	if lineWith(c.scr.lines(), "Replicas") || lineWith(c.scr.lines(), "Scale deployments/") {
		t.Errorf("s opened something for a read-only key:\n%s", c.scr)
	}
	if after := replicas(t); after != before {
		t.Errorf("the replicas of web went from %s to %s", before, after)
	}

	c.send(":secrets" + keyEnter)
	c.waitFor("Secrets(default)")
	c.waitRow("hidden")
	noSecret("the table of secrets")
	for _, key := range []string{"y", "d", "x"} {
		c.send(key)
		c.waitUntil(wait, key+" on a Secret is refused, and the table stays", func(lines []string) bool {
			return lineWith(lines, "this key is read-only") && lineWith(lines, "Secrets(default)") && row(lines, "hidden") != ""
		})
		noSecret(key + " on the Secret")
		// The flash goes after 5 seconds; the next key's own is then
		// to be seen.
		c.waitUntil(wait, "the flash is gone", func(lines []string) bool { return !lineWith(lines, "this key is read-only") })
		if row(c.scr.lines(), "hidden") == "" {
			t.Fatalf("after %s the table of secrets is gone:\n%s", key, c.scr)
		}
		noSecret("after " + key)
	}

	eventually(t, wait, "kosh's log has a refused line", func() bool {
		return len(logLines(t, "refused", "key", k.name)) > 0
	}, func() string { return "kosh's log:\n" + koshLog(t) })
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
	cmd := exec.Command(cfg.irohSSH, "-i", k.file,
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentitiesOnly=yes",
		"kosh@"+cfg.endpoint)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	return startPTY(t, cmd, 120, 40)
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

// Last: it takes the read-only key out of the list.
func TestKeyRemovedEndsTheSession(t *testing.T) {
	inCluster(t)
	full, ro := fullKey(t), readOnlyKey(t)
	f := connect(t, full, 120, 40)
	f.waitFor("Pods(default)", full.name)
	r := connect(t, ro, 120, 40)
	r.waitFor("Pods(default)", ro.name+" [read-only]")

	setKeys(t, full.line)
	t.Cleanup(func() { setKeys(t, full.line, "read-only "+ro.line) })
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

	if _, err := dialSSH(t, ro, 120, 40); err == nil {
		t.Error("the read-only key is still accepted for a new connection")
	}
}

// iroh-ssh finds kosh, which runs as a process on this machine with the
// relays off, by mDNS alone: scripts/test-mdns.sh.
func TestIrohSSHFindsKoshByMDNS(t *testing.T) {
	if !cfg.mdns {
		t.Skip("KOSH_E2E_MDNS is not 1: scripts/test-mdns.sh runs this test")
	}
	need(t, "KOSH_E2E_FULL_KEY", "KOSH_E2E_ENDPOINT", "KOSH_E2E_IROH_SSH", "KOSH_E2E_LOG")
	k := fullKey(t)
	cmd := exec.Command(cfg.irohSSH, "-i", k.file,
		"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "IdentitiesOnly=yes",
		"kosh@"+cfg.endpoint)
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	p := startPTY(t, cmd, 120, 40)
	overIroh(t, p, "the pods, found by mDNS", func(lines []string) bool {
		return lineWith(lines, "Pods(default)") && lineWith(lines, "Key:", k.name)
	})
	p.send(":q" + keyEnter)
	if status := p.exitedWith(wait); status != 0 {
		t.Errorf("iroh-ssh exited with status %d after :q; the screen:\n%s", status, p.scr)
	}
}
