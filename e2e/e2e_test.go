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
// scripts/test-e2e.sh lists three keys, of three roles: the full key
// alice@kosh-e2e/cluster-admin, the view key bob@kosh-e2e/view, and the
// team key carol@kosh-e2e/edit@kosh-e2e-team, of the Role edit the script
// makes in kosh-e2e-team (testdata/0-team.yaml).
//
// The tests of the cluster run in the order of this file, against one
// cluster and one kosh: TestRoleCanBeChanged, TestKeyRemovedEndsTheSession,
// TestShellEndsWithItsKey, TestBindingsExistAndGo and TestOldAdminListWarns
// change kosh's role and its key list, and put them back;
// TestStopTellsTheSessions restarts kosh and comes last.
// The other tests of a shell come before them. The tests of the metrics
// come after the shell's, and TestMetricsColumnsFollowTheAPI, which runs
// only with KOSH_E2E_SLOW=1, takes the metrics API away and puts it back.

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
	viewKey  string // KOSH_E2E_VIEW_KEY: the view key's
	teamKey  string // KOSH_E2E_TEAM_KEY: the team key's
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
	viewKey:   os.Getenv("KOSH_E2E_VIEW_KEY"),
	teamKey:   os.Getenv("KOSH_E2E_TEAM_KEY"),
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
	need(t, "KOSH_E2E_SSH", "KOSH_E2E_FULL_KEY", "KOSH_E2E_VIEW_KEY", "KOSH_E2E_TEAM_KEY", "KOSH_E2E_HOST_KEY",
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

func fullKey(t *testing.T) key { return loadKey(t, cfg.fullKey) }
func viewKey(t *testing.T) key { return loadKey(t, cfg.viewKey) }
func teamKey(t *testing.T) key { return loadKey(t, cfg.teamKey) }

// teamNamespace is the namespace of the team key's Role.
const teamNamespace = "kosh-e2e-team"

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

// lineWithHint returns whether a line shows the hint of key with text
// after it: "<key> text", with the padding that lines a header column's
// texts up between them.
func lineWithHint(lines []string, key, text string) bool {
	re := regexp.MustCompile(regexp.QuoteMeta("<"+key+">") + " +" + regexp.QuoteMeta(text))
	for _, l := range lines {
		if re.MatchString(l) {
			return true
		}
	}
	return false
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
	// The hint a view key is not shown (TestViewHasNoActionKeys).
	c.waitUntil(wait, "the hint <s> Scale", func(lines []string) bool { return lineWithHint(lines, "s", "Scale") })
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

// wideW and wideH are a terminal whose header has room for every column
// of hints of the tables of pods and deployments, the keys of the
// bindings among them.
const wideW, wideH = 240, 40

// keysIn waits until the header shows the hint <d> Describe, which a
// table binds, with the rest of its keys, once the API server has
// answered what the key's role may do there; and returns the header.
func (c *term) keysIn() []string {
	c.t.Helper()
	c.waitUntil(wait, "the hint <d> Describe: the role's answers are in", func(lines []string) bool {
		return lineWithHint(headerLines(lines), "d", "Describe")
	})
	return headerLines(c.scr.lines())
}

// The hints of the keys that change the cluster, on a table of pods and
// on a table of deployments.
var (
	podActions        = [][2]string{{"ctrl-d", "Delete"}, {"e", "Edit"}, {"ctrl-k", "Kill"}, {"s", "Shell"}}
	deploymentActions = [][2]string{{"ctrl-d", "Delete"}, {"e", "Edit"}, {"s", "Scale"}, {"r", "Restart"}, {"i", "Set Image"}}
)

// hasHints fails the test unless the header has every one of hints, or,
// with want false, none of them.
func hasHints(t *testing.T, who string, header []string, hints [][2]string, want bool) {
	t.Helper()
	for _, h := range hints {
		if lineWithHint(header, h[0], h[1]) != want {
			t.Errorf("%s: the hint <%s> %s shown %v, want %v:\n%s", who, h[0], h[1], !want, want, strings.Join(header, "\n"))
		}
	}
}

// A view key sees the cluster and has none of the keys that change it:
// the header names the key, role and all, without [read-only], and s on
// a deployment does nothing. The full key, on a terminal as wide, is the
// control: its header has every one of those keys.
func TestViewHasNoActionKeys(t *testing.T) {
	inCluster(t)
	full, k := fullKey(t), viewKey(t)
	a := connect(t, full, wideW, wideH)
	a.waitRow("marker", "Running")
	hasHints(t, full.name+" on pods", a.keysIn(), podActions, true)
	a.send(":deploy" + keyEnter)
	a.waitRow("web")
	hasHints(t, full.name+" on deployments", a.keysIn(), deploymentActions, true)

	c := connect(t, k, wideW, wideH)
	c.waitRow("marker", "Running")
	header := c.keysIn()
	if !lineWith(header, "Key:", k.name) || lineWith(header, "[read-only]") {
		t.Errorf("the header does not name %s alone:\n%s", k.name, strings.Join(header, "\n"))
	}
	hasHints(t, k.name+" on pods", header, podActions, false)
	c.send(":deploy" + keyEnter)
	c.waitRow("web")
	hasHints(t, k.name+" on deployments", c.keysIn(), deploymentActions, false)

	before := replicas(t)
	// What TestSSHScales types to scale web to 3.
	c.send("s")
	c.send(scaleTo3...)
	c.send(scaleConfirm...)
	// Nothing is to be waited for: what s would open or do would be there
	// by now.
	time.Sleep(2 * time.Second)
	if lineWith(c.scr.lines(), "Replicas") || lineWith(c.scr.lines(), "Scale deployments/") {
		t.Errorf("s opened something for a view key:\n%s", c.scr)
	}
	if after := replicas(t); after != before {
		t.Errorf("the replicas of web went from %s to %s", before, after)
	}
	if lines := logLines(t, "action", "key", k.name, "action", "scale"); len(lines) > 0 {
		t.Errorf("kosh's log has a scale of the view key: %s", lines[0])
	}
}

// joined is the screen's lines, trimmed, in one line: a message the flash
// wrapped, whole.
func joined(lines []string) string {
	var trimmed []string
	for _, l := range lines {
		trimmed = append(trimmed, strings.TrimSpace(l))
	}
	return strings.Join(trimmed, " ")
}

// A view key cannot read a Secret: view grants nothing of secrets, and
// the API server refuses the key the table, which says so; the keys that
// would show a Secret are not bound, and its value reaches the terminal
// in no form. kubectl, as the key's user, is refused the Secret itself.
func TestViewCannotDecodeASecret(t *testing.T) {
	inCluster(t)
	k := viewKey(t)
	c := connect(t, k, wideW, wideH)
	c.waitFor("Pods(default)")
	// From here on, every write to the terminal is looked at for the
	// Secret's value in each of its forms, to the end of the test and
	// what is received while it ends.
	c.scr.watch(secretForms...)
	t.Cleanup(func() {
		if seen := c.scr.seen(); len(seen) > 0 {
			t.Errorf("by the end: the Secret's value reached the terminal: %s", strings.Join(seen, "\n"))
		}
	})

	c.send(":secrets default" + keyEnter)
	c.waitUntil(wait, "the table of secrets with the API's refusal", func(lines []string) bool {
		all := joined(lines)
		return lineWith(lines, "Secrets(default)") && strings.Contains(all, "forbidden") && strings.Contains(all, `cannot list resource "secrets"`)
	})
	noSecret(t, &c.term, "the table of secrets")
	if row(c.scr.lines(), "hidden") != "" {
		t.Errorf("a view key has the row of the Secret hidden:\n%s", c.scr)
	}
	hasHints(t, k.name+" on secrets", headerLines(c.scr.lines()), [][2]string{{"x", "Decode"}, {"y", "YAML"}, {"d", "Describe"}}, false)
	for _, key := range []string{"y", "d", "x"} {
		c.send(key)
	}
	// Nothing is to be waited for: what the keys would open would be
	// there by now.
	time.Sleep(2 * time.Second)
	noSecret(t, &c.term, "after y, d and x")
	if lineWith(c.scr.lines(), "this key is read-only") {
		t.Errorf("kosh refused the key itself, as before roles:\n%s", c.scr)
	}

	// The API server decides: it refuses the key's user, in the role's
	// group, the Secret.
	out, err := run("kubectl", "--namespace", "default", "get", "secret", "hidden", "-o", "yaml",
		"--as", "kosh:"+k.name, "--as-group", "kosh:clusterrole:view")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Errorf("kubectl as the view key's user: %v, want forbidden", err)
	}
	for _, f := range secretForms {
		if strings.Contains(out, f) || (err != nil && strings.Contains(err.Error(), f)) {
			t.Errorf("kubectl as the view key's user was given the Secret's value")
		}
	}
	noSecret(t, &c.term, "at the end")
}

// A key with a Role starts in the Role's namespace, on its pods; its
// namespace shortcuts are all and that namespace, without default; and
// its header has no CPU and MEM lines, since a Role cannot grant nodes.
func TestRoleStartsInItsNamespace(t *testing.T) {
	inCluster(t)
	k := teamKey(t)
	c := connect(t, k, wideW, wideH)
	c.waitFor("Pods(" + teamNamespace + ")")
	c.waitRow("teamwork", "Running")
	c.waitUntil(wait, "the key's name and the shortcuts <0> all and <1> "+teamNamespace, func(lines []string) bool {
		h := headerLines(lines)
		return lineWith(h, "Key:", k.name) && lineWithHint(h, "0", "all") && lineWithHint(h, "1", teamNamespace)
	})
	c.keysIn()
	// The lines of a ClusterRole's key come with the first reading of the
	// metrics, at once (TestHeaderShowsClusterUsage).
	time.Sleep(2 * time.Second)
	header := headerLines(c.scr.lines())
	if lineWith(header, "default") {
		t.Errorf("default is among the shortcuts of a Role's key:\n%s", strings.Join(header, "\n"))
	}
	if lineWith(header, "CPU:") || lineWith(header, "MEM:") {
		t.Errorf("the header of a Role's key has the cluster's CPU and MEM:\n%s", strings.Join(header, "\n"))
	}
}

// A key of the Role edit in kosh-e2e-team has the keys its Role allows
// there, and none in default, where the API refuses it the pods.
func TestRoleHasDeleteOnlyInItsNamespace(t *testing.T) {
	inCluster(t)
	k := teamKey(t)
	c := connect(t, k, wideW, wideH)
	c.waitRow("teamwork", "Running")
	header := c.keysIn()
	// The Role has delete on pods, and neither update nor pods/exec.
	hasHints(t, k.name+" in "+teamNamespace, header, [][2]string{{"ctrl-d", "Delete"}, {"ctrl-k", "Kill"}}, true)
	hasHints(t, k.name+" in "+teamNamespace, header, [][2]string{{"e", "Edit"}, {"s", "Shell"}}, false)

	c.send(":pods default" + keyEnter)
	c.waitUntil(wait, "the pods of default with the API's refusal", func(lines []string) bool {
		return lineWith(lines, "Pods(default)") && strings.Contains(joined(lines), "forbidden")
	})
	// Nothing is to be waited for: no answer is yes in default, and the
	// answers of the namespace the table left are forgotten.
	time.Sleep(2 * time.Second)
	if row(c.scr.lines(), "marker") != "" {
		t.Errorf("the team key sees the pods of default:\n%s", c.scr)
	}
	hasHints(t, k.name+" in default", headerLines(c.scr.lines()), append(slices.Clone(podActions), [2]string{"d", "Describe"}), false)

	c.send(":pods " + teamNamespace + keyEnter)
	c.waitRow("teamwork")
	hasHints(t, k.name+" back in "+teamNamespace, c.keysIn(), [][2]string{{"ctrl-d", "Delete"}}, true)
}

// A key with a Role is refused every namespace at once, and the
// namespaces: 0 and :ns show the API's refusal, whole, and 1 is its
// namespace again.
func TestAllNamespacesRefusedToARole(t *testing.T) {
	inCluster(t)
	k := teamKey(t)
	c := connect(t, k, wideW, wideH)
	c.waitRow("teamwork", "Running")

	refused := func(title string) {
		t.Helper()
		c.waitUntil(wait, title+" with the API's refusal", func(lines []string) bool {
			all := joined(lines)
			return lineWith(lines, title) && strings.Contains(all, "forbidden") && strings.Contains(all, "at the cluster scope")
		})
		for _, l := range c.scr.lines() {
			if strings.Contains(l, "forbidden") && strings.HasSuffix(strings.TrimRight(l, " "), "…") {
				t.Errorf("the refusal is cut: %q\n%s", l, c.scr)
			}
		}
		if row(c.scr.lines(), "teamwork") != "" || row(c.scr.lines(), "marker") != "" {
			t.Errorf("%s has rows for the team key:\n%s", title, c.scr)
		}
	}
	c.send("0")
	refused("Pods(all)")
	c.send(":ns" + keyEnter)
	refused("Namespaces(all)")
	c.send(":pods" + keyEnter)
	c.waitFor("Pods(all)")
	c.send("1")
	c.waitFor("Pods(" + teamNamespace + ")")
	c.waitRow("teamwork", "Running")
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
	c := connect(t, viewKey(t), 120, 40)
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

func TestIrohSSHView(t *testing.T) {
	inCluster(t)
	k := viewKey(t)
	p := irohSSH(t, k)
	overIroh(t, p, "the view key in the header", func(lines []string) bool {
		return lineWith(lines, "Pods(default)") && lineWith(lines, "Key:", k.name) && !lineWith(lines, "[read-only]")
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
	p.waitUntil(wait, "the hint <s> Shell", func(lines []string) bool { return lineWithHint(headerLines(lines), "s", "Shell") })
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

// A view key has no shell: view grants no pods/exec, so no hint, and s
// does nothing. TestShell is the positive control: a full key sees <s>
// Shell, at the same width.
func TestShellViewKey(t *testing.T) {
	inCluster(t)
	k := viewKey(t)
	c := connect(t, k, 120, 40)
	c.waitUntil(wait, "the key's name in the header", func(lines []string) bool {
		return lineWith(lines, "Key:", k.name)
	})
	c.waitRow("marker", "Running")
	c.fills(120, 40)
	c.keysIn()
	before := c.scr.lines()
	if lineWithHint(headerLines(before), "s", "Shell") {
		t.Errorf("a view key is offered <s> Shell:\n%s", c.scr)
	}
	c.send("s")
	// Nothing is to be waited for: what s would open would be there by now.
	time.Sleep(2 * time.Second)
	after := c.scr.lines()
	if dialogLines(after) != nil || lineWith(after, "A shell in pod") {
		t.Errorf("s opened something for a view key:\n%s", c.scr)
	}
	// The frame is as it was: the header, the table's title, the crumbs
	// and the flash. (The table's ages may have moved on.)
	same := func(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }
	// The table's top line, with its title: the first line after the
	// header, which fills found in before.
	top := func(lines []string) string {
		if n := len(headerLines(lines)); n < len(lines) {
			return lines[n]
		}
		t.Fatalf("after s the frame has no table:\n%s", c.scr)
		return ""
	}
	h := len(after)
	if !same(headerLines(after), headerLines(before)) || !same(after[h-2:], before[h-2:]) || top(after) != top(before) {
		t.Errorf("the frame changed after s.\nbefore:\n%s\nafter:\n%s", strings.Join(before, "\n"), c.scr)
	}
	if lines := logLines(t, "action", "key", k.name, "action", "exec"); len(lines) > 0 {
		t.Errorf("kosh's log has an exec of the view key: %s", lines[0])
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
	eventually(t, wait, "kosh's log has the end of the session of "+remote[1]+" and of its shell", func() bool {
		ended = logLines(t, "action", end...)
		return len(logLines(t, "session end", "remote", remote[1])) > 0 && len(ended) > shells
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	t.Logf("%v after the kill: %s", time.Since(killed).Round(100*time.Millisecond), logLines(t, "session end", "remote", remote[1])[0])
	shell := ended[len(ended)-1]
	t.Logf("and: %s", shell)
	// The stream was cut, not the shell ended: the spec's §7.
	if !hasAttr(shell, "ended", "the session ended") || strings.Contains(shell, " error=") {
		t.Errorf("the shell's end line does not say ended=%q, and that alone: %s", "the session ended", shell)
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

// tableHeader returns the index of the table's header line, the line
// under its top border, or -1.
func tableHeader(lines []string) int {
	for i, l := range lines {
		if strings.HasPrefix(l, "┌") && i+1 < len(lines) {
			return i + 1
		}
	}
	return -1
}

// words returns where each run of characters that are not blank or the
// table's border is in line, in runes: [from, to) pairs.
func words(line []rune) [][2]int {
	var out [][2]int
	from := -1
	for i := 0; i <= len(line); i++ {
		blank := i == len(line) || line[i] == ' ' || line[i] == '│'
		switch {
		case !blank && from < 0:
			from = i
		case blank && from >= 0:
			out = append(out, [2]int{from, i})
			from = -1
		}
	}
	return out
}

// column returns where the column name is in the table's header line, in
// runes, with the sort arrow after it if it has one; ok is false when the
// table has no such column.
func column(lines []string, name string) (from, to int, ok bool) {
	h := tableHeader(lines)
	if h < 0 {
		return 0, 0, false
	}
	header := []rune(lines[h])
	for _, w := range words(header) {
		if strings.TrimRight(string(header[w[0]:w[1]]), "↑↓") == name {
			return w[0], w[1], true
		}
	}
	return 0, 0, false
}

// hasColumn reports whether the table's header line has the column name.
func hasColumn(lines []string, name string) bool {
	_, _, ok := column(lines, name)
	return ok
}

// cell returns the cell of the column name in the row whose first cell
// is first: the text of the row under the column's name in the header
// line, where a cell aligned to the left begins and one aligned to the
// right ends. ok is false when the table has no such column or no such
// row; a blank cell is "".
func cell(lines []string, first, name string) (string, bool) {
	from, to, ok := column(lines, name)
	if !ok {
		return "", false
	}
	for _, l := range lines[tableHeader(lines)+1:] {
		cells := strings.Fields(strings.Trim(l, "│ "))
		if len(cells) == 0 || cells[0] != first {
			continue
		}
		r := []rune(l)
		for _, w := range words(r) {
			if w[0] < to && w[1] > from {
				return string(r[w[0]:w[1]]), true
			}
		}
		return "", true
	}
	return "", false
}

// number matches a cell of millicores, mebibytes or a percentage: a
// whole number.
var number = regexp.MustCompile(`^[0-9]+$`)

// The cluster of scripts/test-e2e.sh has metrics-server: the pods table
// has the metric columns, and the running pod a number in CPU once
// metrics-server has measured it.
func TestPodsShowCPUAndMemory(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 160, 40)
	c.waitFor("Pods(default)")
	c.waitRow("marker", "Running")
	c.waitUntil(wait, "the columns CPU and MEM", func(lines []string) bool {
		return hasColumn(lines, "CPU") && hasColumn(lines, "MEM")
	})
	// n/a until a reading covers the pod, then a number. What it showed
	// on the way is logged, not checked: a frame can be read half drawn.
	var seen []string
	c.waitUntil(90*time.Second, "a number in marker's CPU", func(lines []string) bool {
		v, _ := cell(lines, "marker", "CPU")
		if len(seen) == 0 || seen[len(seen)-1] != v {
			seen = append(seen, v)
		}
		return number.MatchString(v)
	})
	t.Logf("marker's CPU was in turn %q", seen)
	c.waitUntil(wait, "a number in marker's MEM", func(lines []string) bool {
		v, _ := cell(lines, "marker", "MEM")
		return number.MatchString(v)
	})
	t.Logf("the screen:\n%s", c.scr)
}

// nodeName is the name of the kind cluster's one node.
func nodeName(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(kubectl(t, "get", "nodes", "-o", "jsonpath={.items[0].metadata.name}"))
}

// A node's allocatable CPU is the node's own, without a reading.
func TestNodesShowAllocatable(t *testing.T) {
	inCluster(t)
	node := nodeName(t)
	c := connect(t, fullKey(t), 160, 40)
	c.waitFor("Pods(default)")
	c.send(":nodes" + keyEnter)
	c.waitRow(node)
	c.waitUntil(wait, "a number in "+node+"'s CPU/A", func(lines []string) bool {
		v, _ := cell(lines, node, "CPU/A")
		return number.MatchString(v)
	})
	for _, name := range []string{"CPU", "%CPU", "MEM", "MEM/A", "%MEM"} {
		if !hasColumn(c.scr.lines(), name) {
			t.Errorf("the nodes table has no column %s:\n%s", name, c.scr)
		}
	}
	t.Logf("the screen:\n%s", c.scr)
}

// usageLine matches the header's line of the cluster's CPU or MEM with a
// percentage.
var usageLine = regexp.MustCompile(`^(CPU|MEM): +[0-9]+%`)

// usageLines returns the header's lines of CPU and MEM with a
// percentage, by their name.
func usageLines(lines []string) map[string]string {
	found := map[string]string{}
	for _, l := range headerLines(lines) {
		if m := usageLine.FindStringSubmatch(l); m != nil {
			found[m[1]] = l
		}
	}
	return found
}

func TestHeaderShowsClusterUsage(t *testing.T) {
	inCluster(t)
	c := connect(t, fullKey(t), 120, 40)
	c.waitFor("Pods(default)")
	c.waitUntil(90*time.Second, "the header's CPU: and MEM: with a percentage", func(lines []string) bool {
		return len(usageLines(lines)) == 2
	})
	t.Logf("the screen:\n%s", c.scr)
}

// metricsAPIUp waits for the metrics API to be available again.
func metricsAPIUp(t *testing.T) {
	t.Helper()
	kubectl(t, "wait", "--for=condition=Available", "apiservice/v1beta1.metrics.k8s.io", "--timeout=180s")
}

// saveMetricsAPI writes the APIService of the metrics API, as the
// cluster has it now and without what the API server keeps of it, to a
// file that kubectl apply makes it again from, and returns the file.
func saveMetricsAPI(t *testing.T) string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(kubectl(t, "get", "apiservice", "v1beta1.metrics.k8s.io", "-o", "json")), &obj); err != nil {
		t.Fatal(err)
	}
	delete(obj, "status")
	meta, _ := obj["metadata"].(map[string]any)
	for _, f := range []string{"managedFields", "resourceVersion", "uid", "creationTimestamp", "generation"} {
		delete(meta, f)
	}
	if ann, ok := meta["annotations"].(map[string]any); ok {
		delete(ann, "kubectl.kubernetes.io/last-applied-configuration")
	}
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "apiservice.json")
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// metricsServer returns the generation of the Deployment metrics-server
// and its container's arguments.
func metricsServer(t *testing.T) (generation, args string) {
	t.Helper()
	out := kubectl(t, "--namespace", "kube-system", "get", "deployment", "metrics-server",
		"-o", "jsonpath={.metadata.generation} {.spec.template.spec.containers[0].args}")
	generation, args, _ = strings.Cut(out, " ")
	return generation, args
}

// The columns and the header's lines exist while the cluster serves the
// metrics API: they go within discovery's interval of its removal and
// come back within it of its return. The APIService alone is taken away
// and made again: metrics-server itself runs on, untouched. Up to 12
// minutes, so only with KOSH_E2E_SLOW=1.
func TestMetricsColumnsFollowTheAPI(t *testing.T) {
	inCluster(t)
	if os.Getenv("KOSH_E2E_SLOW") != "1" {
		t.Skip("KOSH_E2E_SLOW=1: it waits for discovery, every 5 minutes, twice")
	}
	saved := saveMetricsAPI(t)
	generation, args := metricsServer(t)
	if !strings.Contains(args, "--kubelet-insecure-tls") {
		t.Fatalf("metrics-server runs without --kubelet-insecure-tls: %s", args)
	}
	// The test puts the API back however it ends.
	t.Cleanup(func() {
		kubectl(t, "apply", "-f", saved)
		metricsAPIUp(t)
	})

	c := connect(t, fullKey(t), 160, 40)
	c.waitFor("Pods(default)")
	c.waitRow("marker", "Running")
	c.waitUntil(wait, "the column CPU and the header's lines", func(lines []string) bool {
		return hasColumn(lines, "CPU") && len(usageLines(lines)) == 2
	})

	start := time.Now()
	kubectl(t, "delete", "apiservice", "v1beta1.metrics.k8s.io")
	c.waitUntil(6*time.Minute, "the column CPU gone", func(lines []string) bool {
		return hasColumn(lines, "NAME") && !hasColumn(lines, "CPU") && row(lines, "marker", "Running") != ""
	})
	t.Logf("the column CPU went %v after the APIService was deleted", time.Since(start).Round(time.Second))
	c.waitUntil(wait, "the header's CPU: and MEM: gone", func(lines []string) bool {
		return !lineWith(headerLines(lines), "CPU:") && !lineWith(headerLines(lines), "MEM:")
	})
	t.Logf("the header's lines went %v after the APIService was deleted", time.Since(start).Round(time.Second))

	start = time.Now()
	kubectl(t, "apply", "-f", saved)
	c.waitUntil(6*time.Minute, "the column CPU back", func(lines []string) bool {
		return hasColumn(lines, "CPU") && row(lines, "marker", "Running") != ""
	})
	t.Logf("the column CPU came back %v after the APIService was made again", time.Since(start).Round(time.Second))
	c.waitUntil(90*time.Second, "the header's CPU: and MEM: back, with a percentage", func(lines []string) bool {
		return len(usageLines(lines)) == 2
	})
	t.Logf("the header's lines came back, with a percentage, %v after the APIService was made again", time.Since(start).Round(time.Second))

	// What came back is the metrics-server that was there: its Deployment
	// was not changed, keeps its flag, and has no rollout under way.
	if g, a := metricsServer(t); g != generation || !strings.Contains(a, "--kubelet-insecure-tls") {
		t.Errorf("metrics-server's Deployment changed: generation %s, arguments %s; before: generation %s, arguments %s", g, a, generation, args)
	}
	kubectl(t, "--namespace", "kube-system", "rollout", "status", "deployment/metrics-server", "--timeout=60s")
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

// koshMay is kubectl auth can-i for kosh's ServiceAccount: "yes" or "no".
func koshMay(t *testing.T, args ...string) string {
	t.Helper()
	out, _ := run("kubectl", append(append([]string{"auth", "can-i"}, args...), "--as", "system:serviceaccount:"+namespace+":"+release)...)
	return strings.TrimSpace(out)
}

// koshMayImpersonate is whether kosh's ServiceAccount may impersonate the
// users of the keys: "yes" or "no".
func koshMayImpersonate(t *testing.T) string { return koshMay(t, "impersonate", "users") }

// setRole installs the chart again with rbac.clusterRole, keeping every
// other value.
func setRole(t *testing.T, role string) {
	t.Helper()
	if _, err := run("helm", "upgrade", release, chart, "--namespace", namespace, "--reuse-values",
		"--set", "rbac.clusterRole="+role); err != nil {
		t.Fatal(err)
	}
}

// The ClusterRole of kosh's ServiceAccount can be changed with helm
// upgrade. One that may not impersonate, as view, leaves every key
// without its role's rights: the full key's session is given no key that
// changes the cluster, and kosh's log says that the key's rights could
// not be checked.
func TestRoleCanBeChanged(t *testing.T) {
	inCluster(t)
	k := fullKey(t)
	setRole(t, "view")
	t.Cleanup(func() {
		setRole(t, "cluster-admin")
		eventually(t, wait, "cluster-admin is kosh's again", func() bool {
			return bindingRole(t, release+"-cluster-admin") == "cluster-admin" && bindingRole(t, release+"-view") == "" &&
				koshMayImpersonate(t) == "yes"
		}, func() string { return "can-i impersonate: " + koshMayImpersonate(t) })
	})
	if got := bindingRole(t, release+"-view"); got != "view" {
		t.Errorf("the binding %s-view binds %q, want view", release, got)
	}
	if got := bindingRole(t, release+"-cluster-admin"); got != "" {
		t.Errorf("the binding %s-cluster-admin is still there, binding %s", release, got)
	}
	eventually(t, wait, "kosh may no longer impersonate", func() bool { return koshMayImpersonate(t) == "no" },
		func() string { return "can-i impersonate: " + koshMayImpersonate(t) })

	warned := len(logLines(t, "the key's rights could not be checked", "key", k.name))
	before := replicas(t)
	c := connect(t, k, wideW, wideH)
	c.waitFor("Pods(default)")
	c.send(":deploy" + keyEnter)
	c.waitFor("Deployments(default)")
	eventually(t, wait, "kosh's log says that the key's rights could not be checked", func() bool {
		return len(logLines(t, "the key's rights could not be checked", "key", k.name)) > warned
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	// The answers are in: no key is bound on a "no".
	time.Sleep(time.Second)
	hasHints(t, k.name+" on deployments, kosh's ServiceAccount view", headerLines(c.scr.lines()),
		append(slices.Clone(deploymentActions), [2]string{"d", "Describe"}), false)
	c.send("s")
	c.send(scaleTo3...)
	c.send(scaleConfirm...)
	time.Sleep(2 * time.Second)
	if lineWith(c.scr.lines(), "Scale deployments/") {
		t.Errorf("s opened a scale with kosh's ServiceAccount view:\n%s", c.scr)
	}
	if after := replicas(t); after != before {
		t.Errorf("the replicas of web went from %s to %s with kosh's ServiceAccount view", before, after)
	}
}

// It takes the view key and the team key out of the list, and puts them
// back.
func TestKeyRemovedEndsTheSession(t *testing.T) {
	inCluster(t)
	full, ro, team := fullKey(t), viewKey(t), teamKey(t)
	f := connect(t, full, 120, 40)
	f.waitFor("Pods(default)", full.name)
	r := connect(t, ro, 120, 40)
	r.waitFor("Pods(default)", ro.name)

	setKeys(t, full.line)
	t.Cleanup(func() {
		// Back as it was, and waited for, so that a test after this one
		// does not meet the kubelet's delay.
		setKeys(t, full.line, ro.line, team.line)
		var err error
		eventually(t, 150*time.Second, "the view key is accepted again", func() bool {
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
	t.Logf("the view key's session ended %v after the list changed", time.Since(changed).Round(100*time.Millisecond))
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 1 {
		t.Errorf("the view key's session ended with %v, want status 1", err)
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
		t.Error("the view key is still accepted for a new connection")
	} else if !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("a new connection of the view key: %v, want a failed handshake", err)
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

// accepted reports whether kosh accepts k: whether the SSH handshake
// as k succeeds, which is where kosh checks the key; no session is
// opened. A handshake that fails for another reason than the key's
// refusal fails the test.
func accepted(t *testing.T, k key) bool {
	t.Helper()
	client, err := ssh.Dial("tcp", cfg.sshAddr, &ssh.ClientConfig{
		User:            "kosh",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(k.signer)},
		HostKeyCallback: ssh.FixedHostKey(hostKey(t)),
		Timeout:         wait,
	})
	switch {
	case err == nil:
		_ = client.Close()
		return true
	case strings.Contains(err.Error(), "unable to authenticate"):
		return false
	}
	t.Fatalf("connecting as %s: %v, which is not a refusal of the key", k.name, err)
	return false
}

// waitAccepted asks kosh once a second whether it accepts k, until the
// answer is want, for within at most: the kubelet updates the mounted
// key list in its own time.
func waitAccepted(t *testing.T, k key, want bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for accepted(t, k) != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: accepted is not %v after %v", k.name, want, within)
		}
		time.Sleep(time.Second)
	}
}

// A shell ends with its session when its key leaves the list. What it
// ran in the container may go on (the spec's §7): the test says whether
// it did, and does not require either.
func TestShellEndsWithItsKey(t *testing.T) {
	inCluster(t)
	full, ro, team := fullKey(t), viewKey(t), teamKey(t)
	k := newKey(t, "erin@kosh-e2e/cluster-admin")
	setKeys(t, full.line, ro.line, team.line, k.line)
	t.Cleanup(func() {
		// The list as it was, and waited for.
		setKeys(t, full.line, ro.line, team.line)
		waitAccepted(t, k, false, 150*time.Second)
	})
	waitAccepted(t, k, true, 150*time.Second)

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

	setKeys(t, full.line, ro.line, team.line)
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
	if !hasAttr(shell, "ended", "the session ended") || strings.Contains(shell, " error=") {
		t.Errorf("the shell's end line does not say ended=%q, and that alone: %s", "the session ended", shell)
	}
	// A record, for whoever reads the run: the session's end, and the
	// close of its connection, by the client or by kosh a second later.
	fp := ssh.FingerprintSHA256(k.signer.PublicKey())
	var closed []string
	deadline := time.Now().Add(10 * time.Second)
	for len(closed) == 0 && time.Now().Before(deadline) {
		closed = logLines(t, "connection closed", "fingerprint", fp)
		time.Sleep(100 * time.Millisecond)
	}
	if ends := logLines(t, "session end", "key", k.name); len(ends) > 0 {
		t.Logf("kosh's log: %s", ends[len(ends)-1])
	}
	for _, l := range closed {
		t.Logf("kosh's log: %s", l)
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

// rbacBinding is a ClusterRoleBinding or a RoleBinding, as kubectl has
// it, with what the tests look at.
type rbacBinding struct {
	Metadata struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	RoleRef struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"roleRef"`
	Subjects []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"subjects"`
}

// getBinding returns the binding of kind (clusterrolebinding or
// rolebinding) named name, in namespace ns for a RoleBinding, or nil when
// there is none.
func getBinding(t *testing.T, kind, ns, name string) *rbacBinding {
	t.Helper()
	args := []string{"get", kind, name, "--ignore-not-found", "-o", "json"}
	if ns != "" {
		args = append(args, "--namespace", ns)
	}
	out := kubectl(t, args...)
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var b rbacBinding
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("%s %s: %v", kind, name, err)
	}
	return &b
}

// koshBinding is a binding kosh makes for a role of its key list (§4 of
// the roles design).
type koshBinding struct {
	kind, ns, name string // as getBinding takes them
	roleKind, role string // the roleRef
	group          string // the one subject, a Group
	annotation     string // kosh.fables-for-robots.dev/role
}

// The bindings of the three keys of scripts/test-e2e.sh.
var (
	fullBinding = koshBinding{"clusterrolebinding", "", "kosh:clusterrole:cluster-admin", "ClusterRole", "cluster-admin", "kosh:clusterrole:cluster-admin", "ClusterRole/cluster-admin"}
	viewBinding = koshBinding{"clusterrolebinding", "", "kosh:clusterrole:view", "ClusterRole", "view", "kosh:clusterrole:view", "ClusterRole/view"}
	teamBinding = koshBinding{"rolebinding", teamNamespace, "kosh:role:edit", "Role", "edit", "kosh:role:" + teamNamespace + ":edit", "Role/" + teamNamespace + "/edit"}
)

// is reports what of b differs from what kosh makes for w, or "".
func (w koshBinding) is(b *rbacBinding) string {
	switch {
	case b == nil:
		return "there is none"
	case b.RoleRef.Kind != w.roleKind || b.RoleRef.Name != w.role:
		return fmt.Sprintf("its roleRef is %s %s", b.RoleRef.Kind, b.RoleRef.Name)
	case len(b.Subjects) != 1 || b.Subjects[0].Kind != "Group" || b.Subjects[0].Name != w.group:
		return fmt.Sprintf("its subjects are %+v", b.Subjects)
	case b.Metadata.Labels["app.kubernetes.io/managed-by"] != "kosh":
		return fmt.Sprintf("its labels are %v", b.Metadata.Labels)
	case b.Metadata.Annotations["kosh.fables-for-robots.dev/role"] != w.annotation:
		return fmt.Sprintf("its annotations are %v", b.Metadata.Annotations)
	}
	return ""
}

// kosh has made the binding of each role of its key list, with its label;
// a role that leaves the list has its binding deleted, and the bindings
// of the roles still named stay. It takes the team key out of the list,
// and puts it back.
func TestBindingsExistAndGo(t *testing.T) {
	inCluster(t)
	full, ro, team := fullKey(t), viewKey(t), teamKey(t)
	// Made at kosh's start, or again after the list was put back by a
	// test before this one.
	for _, w := range []koshBinding{fullBinding, viewBinding, teamBinding} {
		var why string
		eventually(t, wait, "the binding "+w.name, func() bool {
			why = w.is(getBinding(t, w.kind, w.ns, w.name))
			return why == ""
		}, func() string { return why + "\nkosh's log:\n" + koshLog(t) })
	}

	setKeys(t, full.line, ro.line)
	changed := time.Now()
	t.Cleanup(func() {
		// Back as it was, and waited for.
		setKeys(t, full.line, ro.line, team.line)
		waitAccepted(t, team, true, 150*time.Second)
		eventually(t, wait, "the team key's RoleBinding is made again", func() bool {
			return teamBinding.is(getBinding(t, teamBinding.kind, teamBinding.ns, teamBinding.name)) == ""
		}, func() string { return "kosh's log:\n" + koshLog(t) })
	})
	eventually(t, 90*time.Second, "the team key's RoleBinding is deleted", func() bool {
		return getBinding(t, teamBinding.kind, teamBinding.ns, teamBinding.name) == nil
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	t.Logf("the team key's RoleBinding was deleted %v after the list changed", time.Since(changed).Round(100*time.Millisecond))
	if lines := logLines(t, "a binding no key needs is deleted", "binding", teamNamespace+"/"+teamBinding.name); len(lines) == 0 {
		t.Errorf("kosh's log does not say that it deleted the binding:\n%s", koshLog(t))
	}
	for _, w := range []koshBinding{fullBinding, viewBinding} {
		if why := w.is(getBinding(t, w.kind, w.ns, w.name)); why != "" {
			t.Errorf("the binding %s of a role still named: %s", w.name, why)
		}
	}
	if accepted(t, team) {
		t.Error("the team key is still accepted")
	}
}

// A key list of before 0.5.0 names its admins …/admin, which is now the
// ClusterRole admin: kosh warns of it, with the key, and binds that
// ClusterRole, which gives no node.
func TestOldAdminListWarns(t *testing.T) {
	inCluster(t)
	full, ro, team := fullKey(t), viewKey(t), teamKey(t)
	old := newKey(t, "dave@kosh-e2e/admin")
	setKeys(t, full.line, ro.line, team.line, old.line)
	t.Cleanup(func() {
		// The list as it was, and waited for.
		setKeys(t, full.line, ro.line, team.line)
		waitAccepted(t, old, false, 150*time.Second)
	})
	const warning = "a key's role is the ClusterRole admin, which is not cluster-admin: before 0.5.0 /admin meant full access"
	var warned []string
	eventually(t, 150*time.Second, "kosh's log warns of the ClusterRole admin", func() bool {
		warned = logLines(t, warning, "keys", old.name)
		return len(warned) > 0
	}, func() string { return "kosh's log:\n" + koshLog(t) })
	if !hasAttr(warned[0], "level", "WARN") {
		t.Errorf("the warning is not at warn level: %s", warned[0])
	}
	t.Logf("kosh's log: %s", warned[0])
	adminBinding := koshBinding{"clusterrolebinding", "", "kosh:clusterrole:admin", "ClusterRole", "admin", "kosh:clusterrole:admin", "ClusterRole/admin"}
	var why string
	eventually(t, wait, "the binding of the ClusterRole admin", func() bool {
		why = adminBinding.is(getBinding(t, adminBinding.kind, adminBinding.ns, adminBinding.name))
		return why == ""
	}, func() string { return why })
	out, _ := run("kubectl", "auth", "can-i", "list", "nodes", "--as", "kosh:"+old.name, "--as-group", adminBinding.group)
	if strings.TrimSpace(out) != "no" {
		t.Errorf("the ClusterRole admin may list the nodes: can-i says %q", strings.TrimSpace(out))
	}
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
