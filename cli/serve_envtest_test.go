package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	stdlog "log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/go-logr/logr"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/fables-for-robots/kosh/irohnet"
	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/state"
)

// startAPIServer starts kube-apiserver and etcd for one test, and stops
// them when it ends. It skips the test without KUBEBUILDER_ASSETS.
func startAPIServer(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set: run scripts/envtest-assets.sh")
	}
	// controller-runtime complains on standard error when its log is
	// not set; envtest is all of it the tests use.
	ctrllog.SetLogger(logr.Discard())
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting the API server: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping the API server: %v", err)
		}
	})
	return cfg
}

// kubeconfigOf writes a kubeconfig that reaches the API server of cfg
// and returns its path.
func kubeconfigOf(t *testing.T, cfg *rest.Config) string {
	t.Helper()
	kc := clientcmdapi.NewConfig()
	kc.Clusters["envtest"] = &clientcmdapi.Cluster{Server: cfg.Host, CertificateAuthorityData: cfg.CAData}
	kc.AuthInfos["envtest"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData, ClientKeyData: cfg.KeyData, Token: cfg.BearerToken,
	}
	kc.Contexts["envtest"] = &clientcmdapi.Context{Cluster: "envtest", AuthInfo: "envtest"}
	kc.CurrentContext = "envtest"
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := clientcmd.WriteToFile(*kc, path); err != nil {
		t.Fatal(err)
	}
	return path
}

// user is a client key and its line in the key list.
type user struct {
	signer ssh.Signer
	line   string
}

// newUser makes a key named name, read-only or full.
func newUser(t *testing.T, name string, readOnly bool) user {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + name
	if readOnly {
		line = keys.OptionReadOnly + " " + line
	}
	return user{signer: signer, line: line}
}

// kosh is a serve that runs in the test.
type kosh struct {
	opts    serveOptions
	serving serving
	log     *recorder
	cancel  context.CancelFunc
	done    chan error // receives serve's error
	hostKey ssh.PublicKey
}

// envOptions are the options of a serve against the API server of cfg,
// over TCP on the loopback address, with a key list of the users.
func envOptions(t *testing.T, cfg *rest.Config, users ...user) serveOptions {
	t.Helper()
	var lines []string
	for _, u := range users {
		lines = append(lines, u.line)
	}
	keyList := filepath.Join(t.TempDir(), "authorized_keys")
	writeFile(t, keyList, strings.Join(lines, "\n")+"\n")
	return serveOptions{
		sshListen:      "127.0.0.1:0",
		iroh:           false,
		irohRelay:      true,
		irohMDNS:       true,
		stateDir:       filepath.Join(t.TempDir(), "state"),
		authorizedKeys: keyList,
		kubeconfig:     kubeconfigOf(t, cfg),
	}
}

// startKosh runs serve with o until the test ends, and returns once it
// serves.
func startKosh(t *testing.T, o serveOptions) *kosh {
	t.Helper()
	k := &kosh{opts: o, log: &recorder{}, done: make(chan error, 1)}
	o.log = k.log.logger()
	started := make(chan serving, 1)
	o.started = func(s serving) { started <- s }
	ctx, cancel := context.WithCancel(context.Background())
	k.cancel = cancel
	go func() { k.done <- serve(ctx, o) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-k.done:
		case <-time.After(15 * time.Second):
			t.Errorf("serve has not returned 15 seconds after its context was cancelled")
		}
	})
	select {
	case k.serving = <-started:
	case err := <-k.done:
		t.Fatalf("serve: %v\nlog:\n%s", err, k.log.dump())
	case <-time.After(20 * time.Second):
		t.Fatalf("serve has not started in 20 seconds; log:\n%s", k.log.dump())
	}
	st, err := state.Load(o.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	k.hostKey = st.HostKey.PublicKey()
	return k
}

// screen is what a session has written, in the order it came.
type screen struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	done chan struct{} // closed when the session's output has ended
}

func (s *screen) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

// text returns what was written so far, without escape sequences.
func (s *screen) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ansi.Strip(s.buf.String())
}

// client is an SSH client with one session that has a terminal of 100
// by 30.
type client struct {
	conn   net.Conn
	ssh    *ssh.Client
	sess   *ssh.Session
	stdin  io.WriteCloser
	screen *screen
	waited chan error // receives the session's Wait
}

// connect makes an SSH connection over conn with u's key, checks the
// host key against k's, and opens a session with a terminal.
func connect(k *kosh, conn net.Conn, u user) (*client, error) {
	cfg := &ssh.ClientConfig{
		User:            "kosh",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(u.signer)},
		HostKeyCallback: ssh.FixedHostKey(k.hostKey),
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	sc, chans, reqs, err := ssh.NewClientConn(conn, conn.RemoteAddr().String(), cfg)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	c := &client{conn: conn, ssh: ssh.NewClient(sc, chans, reqs), screen: &screen{done: make(chan struct{})}, waited: make(chan error, 1)}
	if c.sess, err = c.ssh.NewSession(); err != nil {
		_ = c.ssh.Close()
		return nil, err
	}
	if err := c.sess.RequestPty("xterm-256color", 30, 100, ssh.TerminalModes{}); err != nil {
		_ = c.ssh.Close()
		return nil, err
	}
	if c.stdin, err = c.sess.StdinPipe(); err != nil {
		_ = c.ssh.Close()
		return nil, err
	}
	stdout, err := c.sess.StdoutPipe()
	if err != nil {
		_ = c.ssh.Close()
		return nil, err
	}
	go func() {
		_, _ = io.Copy(c.screen, stdout)
		close(c.screen.done)
	}()
	if err := c.sess.Shell(); err != nil {
		_ = c.ssh.Close()
		return nil, err
	}
	go func() { c.waited <- c.sess.Wait() }()
	return c, nil
}

// dialTCP connects to k's SSH port as u and fails the test when it
// cannot.
func dialTCP(t *testing.T, k *kosh, u user) *client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", k.serving.ssh.String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := connect(k, conn, u)
	if err != nil {
		t.Fatalf("connecting: %v\nlog:\n%s", err, k.log.dump())
	}
	t.Cleanup(func() { _ = c.ssh.Close() })
	return c
}

// waitFor waits until the screen holds every one of texts.
func (c *client) waitFor(t *testing.T, texts ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		text := c.screen.text()
		missing := ""
		for _, want := range texts {
			if !strings.Contains(text, want) {
				missing = want
				break
			}
		}
		if missing == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the screen has no %q after 10 seconds:\n%s", missing, text)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// send types s.
func (c *client) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(c.stdin, s); err != nil {
		t.Fatal(err)
	}
}

// ended waits for the session to end and returns its Wait's error.
func (c *client) ended(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case err := <-c.waited:
		return err
	case <-time.After(within):
		t.Fatalf("the session has not ended in %v; the screen:\n%s", within, c.screen.text())
		return nil
	}
}

func TestServeEndToEnd(t *testing.T) {
	cfg := startAPIServer(t)
	alice := newUser(t, "alice@laptop", false)
	k := startKosh(t, envOptions(t, cfg, alice))

	rec, ok := k.log.find("kosh is serving")
	if !ok {
		t.Fatalf("no line kosh is serving:\n%s", k.log.dump())
	}
	if rec.Level != slog.LevelInfo || !strings.HasPrefix(rec.Attrs["cluster"], "v1.") || rec.Attrs["version"] != Version ||
		rec.Attrs["ssh"] != k.serving.ssh.String() || !strings.HasPrefix(rec.Attrs["hostkey"], "SHA256:") {
		t.Errorf("%v", rec)
	}
	if _, ok := rec.Attrs["endpoint"]; ok {
		t.Errorf("%v: an endpoint without iroh", rec)
	}

	c := dialTCP(t, k, alice)
	c.waitFor(t, "Pods(default)", "alice@laptop")
	c.send(t, ":ns\r")
	c.waitFor(t, "Namespaces", "default")
	c.send(t, ":q\r")
	if err := c.ended(t, 5*time.Second); err != nil {
		t.Errorf("the session ended with %v, want status 0", err)
	}

	// The name of a key is what its owner wrote: wherever the log names
	// a key, its fingerprint is beside it.
	fp := ssh.FingerprintSHA256(alice.signer.PublicKey())
	eventually(t, waitTime, "the session's end in the log", func() bool {
		_, ok := k.log.find("session end")
		return ok
	})
	named := 0
	for _, r := range k.log.records() {
		if name, ok := r.Attrs["key"]; ok {
			named++
			if name != "alice@laptop" || r.Attrs["fingerprint"] != fp {
				t.Errorf("%v: a key without its fingerprint %s", r, fp)
			}
		}
	}
	if named == 0 {
		t.Errorf("no line names the key:\n%s", k.log.dump())
	}
}

func TestServeReadOnlyKey(t *testing.T) {
	cfg := startAPIServer(t)
	bob := newUser(t, "bob", true)
	k := startKosh(t, envOptions(t, cfg, bob))
	c := dialTCP(t, k, bob)
	c.waitFor(t, "Pods(default)", "bob [read-only]")
}

func TestServeUnknownKeyIsRefused(t *testing.T) {
	cfg := startAPIServer(t)
	alice := newUser(t, "alice", false)
	k := startKosh(t, envOptions(t, cfg, alice))
	conn, err := net.DialTimeout("tcp", k.serving.ssh.String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connect(k, conn, newUser(t, "mallory", false)); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("a key that is not in the list: %v", err)
	}
}

func TestServeKeyListIsFollowed(t *testing.T) {
	cfg := startAPIServer(t)
	alice := newUser(t, "alice", false)
	carol := newUser(t, "carol", false)
	o := envOptions(t, cfg, alice, carol)
	k := startKosh(t, o)
	c := dialTCP(t, k, alice)
	c.waitFor(t, "Pods(default)", "alice")

	writeFile(t, o.authorizedKeys, carol.line+"\n")
	err := c.ended(t, 5*time.Second)
	var exit *ssh.ExitError
	if !errors.As(err, &exit) || exit.ExitStatus() != 1 {
		t.Errorf("the session ended with %v, want status 1", err)
	}
	<-c.screen.done
	if text := c.screen.text(); !strings.Contains(text, "kosh: this key is no longer accepted") {
		t.Errorf("the session was not told:\n%s", text)
	}
	// The other key is still accepted.
	dialTCP(t, k, carol).waitFor(t, "Pods(default)", "carol")
}

func TestServeStops(t *testing.T) {
	cfg := startAPIServer(t)
	alice := newUser(t, "alice", false)
	k := startKosh(t, envOptions(t, cfg, alice))
	c := dialTCP(t, k, alice)
	c.waitFor(t, "Pods(default)")

	start := time.Now()
	k.cancel()
	select {
	case err := <-k.done:
		if err != nil {
			t.Errorf("serve returned %v after a stop that was asked for", err)
		}
	case <-time.After(stopTimeout):
		t.Fatalf("serve has not returned %v after its context was cancelled", stopTimeout)
	}
	t.Logf("stopped in %v", time.Since(start))
	k.done <- nil // for the cleanup

	select {
	case <-c.screen.done:
	case <-time.After(time.Second):
		t.Error("the session's output has not ended")
	}
	if text := c.screen.text(); !strings.Contains(text, "kosh is shutting down") {
		t.Errorf("the session was not told:\n%s", text)
	}
	if conn, err := net.DialTimeout("tcp", k.serving.ssh.String(), time.Second); err == nil {
		_ = conn.Close()
		t.Error("the SSH port is still open")
	}
	if _, ok := k.log.find("kosh has stopped"); !ok {
		t.Errorf("no line kosh has stopped:\n%s", k.log.dump())
	}
}

func TestServeOverIroh(t *testing.T) {
	cfg := startAPIServer(t)
	alice := newUser(t, "alice", false)
	o := envOptions(t, cfg, alice)
	o.sshListen = ""
	o.iroh = true
	o.irohLocal = true
	k := startKosh(t, o)
	if k.serving.ssh != nil || k.serving.iroh == nil {
		t.Fatalf("serving %+v, want iroh alone", k.serving)
	}
	rec, _ := k.log.find("kosh is serving")
	if rec.Attrs["endpoint"] != k.serving.iroh.ID() {
		t.Errorf("%v: not the endpoint ID %s", rec, k.serving.iroh.ID())
	}
	if _, ok := rec.Attrs["ssh"]; ok {
		t.Errorf("%v: an SSH port without one", rec)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := irohnet.DialLocal(ctx, k.serving.iroh.ID(), k.serving.iroh.LocalAddrs())
	if err != nil {
		t.Fatal(err)
	}
	c, err := connect(k, conn, alice)
	if err != nil {
		t.Fatalf("connecting: %v\nlog:\n%s", err, k.log.dump())
	}
	defer func() { _ = c.ssh.Close() }()
	c.waitFor(t, "Pods(default)", "alice")
	c.send(t, ":q\r")
	if err := c.ended(t, 5*time.Second); err != nil {
		t.Errorf("the session ended with %v, want status 0", err)
	}
}

// sessionGoroutines counts the goroutines that run for a session's
// program, its watches and its log streams: those of the TUI, of
// Bubble Tea, of an informer, and of kube's logs. The Cluster's own,
// which run for the whole process, are not counted.
func sessionGoroutines() (int, string) {
	buf := make([]byte, 4<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	var found []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "kube.(*Cluster).discoverEvery") || strings.Contains(g, "testing.tRunner") {
			continue
		}
		for _, mark := range []string{
			"kosh/tui.", "charm.land/bubbletea", "client-go/tools/cache.", "kosh/kube.(*logStream)", "kosh/kube.(*Cluster).startLogs",
		} {
			if strings.Contains(g, mark) {
				n++
				found = append(found, g)
				break
			}
		}
	}
	return n, strings.Join(found, "\n\n")
}

// kubelet is a kubelet that serves the log of every container: a line,
// then nothing more until the request ends. It counts the logs that are
// being read.
type kubelet struct {
	srv     *httptest.Server
	reading atomic.Int64
	asked   atomic.Int64
}

func newKubelet(t *testing.T) *kubelet {
	t.Helper()
	k := &kubelet{}
	k.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/containerLogs/") {
			http.NotFound(w, r)
			return
		}
		k.asked.Add(1)
		k.reading.Add(1)
		defer k.reading.Add(-1)
		_, _ = io.WriteString(w, "a line of the fake kubelet\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(k.srv.Close)
	return k
}

// runningPod makes a node whose kubelet is k, and a pod web on it whose
// container runs.
func runningPod(t *testing.T, typed kubernetes.Interface, k *kubelet) {
	t.Helper()
	ctx := context.Background()
	port := k.srv.Listener.Addr().(*net.TCPAddr).Port
	node, err := typed.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "127.0.0.1"}}
	node.Status.DaemonEndpoints.KubeletEndpoint.Port = int32(port)
	if _, err := typed.CoreV1().Nodes().UpdateStatus(ctx, node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod, err := typed.CoreV1().Pods("default").Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web"},
		Spec:       corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{Name: "web", Image: "nginx"}}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "web", Image: "nginx", Ready: true,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
	}}
	if _, err := typed.CoreV1().Pods("default").UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A client that vanishes without quitting leaves nothing of its session
// running: the watches and the log stream its views opened end because
// the session's context ends, and kosh serves the next client.
func TestServeVanishedClientLeavesNothing(t *testing.T) {
	cfg := startAPIServer(t)
	kl := newKubelet(t)
	runningPod(t, kubernetes.NewForConfigOrDie(cfg), kl)

	alice := newUser(t, "alice", false)
	o := envOptions(t, cfg, alice)
	// An informer nobody watches is stopped at once, so that a watch
	// the session left open shows as an informer that runs.
	o.idleStop = 50 * time.Millisecond
	k := startKosh(t, o)
	if n, found := sessionGoroutines(); n != 0 {
		t.Fatalf("%d goroutines of a session before any:\n%s", n, found)
	}

	c := dialTCP(t, k, alice)
	c.waitFor(t, "Pods(default)", "web", "Running")
	c.send(t, "l")
	c.waitFor(t, "Logs(", "a line of the fake kubelet")
	if kl.reading.Load() != 1 {
		t.Fatalf("%d logs are read, want 1", kl.reading.Load())
	}
	n, found := sessionGoroutines()
	if !strings.Contains(found, "client-go/tools/cache.") {
		t.Fatalf("no informer runs while the session watches its pods:\n%s", found)
	}
	t.Logf("%d goroutines of the session", n)

	// The client vanishes: its connection closes, and it never quits.
	_ = c.conn.Close()
	deadline := time.Now().Add(waitTime)
	for {
		n, found = sessionGoroutines()
		if n == 0 && kl.reading.Load() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d logs are read and %d goroutines of the session are left %v after its client vanished:\n%s\nlog:\n%s",
				kl.reading.Load(), n, waitTime, found, k.log.dump())
		}
		time.Sleep(10 * time.Millisecond)
	}

	dialTCP(t, k, alice).waitFor(t, "Pods(default)", "web")
}

// httpConns reports whether a goroutine's stack is one of net/http's for
// a connection that is kept open: client-go's transports are the
// process's, and keep their idle connections to the API server after
// a Cluster is closed.
func httpConns(g string) bool {
	return strings.Contains(g, "net/http.(*persistConn)") || strings.Contains(g, "http2.(*ClientConn).readLoop")
}

// startGoroutines counts the goroutines but net/http's connections.
func startGoroutines() (int, string) {
	buf := make([]byte, 4<<20)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	var kept []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if httpConns(g) {
			continue
		}
		n++
		kept = append(kept, g)
	}
	return n, strings.Join(kept, "\n\n")
}

// failsLeavingNothing runs serve with o, which fails at a step after the
// cluster was reached, and checks that it leaves no goroutine and no
// open port behind.
func failsLeavingNothing(t *testing.T, o serveOptions, want string) {
	t.Helper()
	before, _ := startGoroutines()
	o.log = (&recorder{}).logger()
	err := serve(context.Background(), o)
	if err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatalf("error %v, want one that begins %q", err, want)
	}
	deadline := time.Now().Add(waitTime)
	for {
		n, all := startGoroutines()
		if n <= before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines, %d before:\n%s", n, before, all)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeFailsOnPortInUse(t *testing.T) {
	cfg := startAPIServer(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	o := envOptions(t, cfg, newUser(t, "alice", false))
	o.sshListen = l.Addr().String()
	failsLeavingNothing(t, o, "the SSH port could not be bound")
}

// The iroh endpoint is bound after the SSH port, whose Serve runs by
// then: both are stopped again.
func TestServeFailsWhenIrohCannotBind(t *testing.T) {
	cfg := startAPIServer(t)
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	o := envOptions(t, cfg, newUser(t, "alice", false))
	o.sshListen = freePort(t)
	o.iroh, o.irohLocal, o.irohBind = true, true, pc.LocalAddr().String()
	failsLeavingNothing(t, o, "the iroh endpoint could not be bound")
	portIsFree(t, o.sshListen)
}

// failingListener is a listener whose Accept fails, once it is told to,
// with err: one that is not temporary, or net.ErrClosed as if it had
// been closed.
type failingListener struct {
	net.Listener
	err  error
	fail chan struct{}
}

var errAcceptBroke = errors.New("the listener broke")

func (l *failingListener) Accept() (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	got := make(chan result, 1)
	go func() {
		c, err := l.Listener.Accept()
		got <- result{c, err}
	}()
	select {
	case r := <-got:
		return r.c, r.err
	case <-l.fail:
		return nil, l.err
	}
}

// listenerEnds runs serve with a TCP listener that fails with err once
// a session runs, and returns serve's error. Everything is stopped as
// for a stop that was asked for.
func listenerEnds(t *testing.T, err error) error {
	t.Helper()
	cfg := startAPIServer(t)
	alice := newUser(t, "alice", false)
	o := envOptions(t, cfg, alice)
	fl := &failingListener{err: err, fail: make(chan struct{})}
	o.wrapTCP = func(l net.Listener) net.Listener {
		fl.Listener = l
		return fl
	}
	k := startKosh(t, o)
	c := dialTCP(t, k, alice)
	c.waitFor(t, "Pods(default)")

	close(fl.fail)
	var got error
	select {
	case got = <-k.done:
	case <-time.After(stopTimeout):
		t.Fatalf("serve has not returned %v after its listener failed", stopTimeout)
	}
	k.done <- nil // for the cleanup
	if err := c.ended(t, time.Second); err == nil {
		t.Error("the session ended with status 0")
	}
	if text := c.screen.text(); !strings.Contains(text, "kosh is shutting down") {
		t.Errorf("the session was not told:\n%s", text)
	}
	if _, ok := k.log.find("kosh has stopped"); !ok {
		t.Errorf("no line kosh has stopped:\n%s", k.log.dump())
	}
	return got
}

// A listener that fails ends serve with its error.
func TestServeListenerFails(t *testing.T) {
	err := listenerEnds(t, errAcceptBroke)
	if !errors.Is(err, errAcceptBroke) || !strings.HasPrefix(err.Error(), "the ssh listener stopped: ") {
		t.Errorf("serve returned %v, want the listener's error", err)
	}
}

// A listener that ends as if it had been closed, which Serve takes for
// a stop and returns nil for, ends serve too: kosh does not go on
// without a transport.
func TestServeListenerStopsByItself(t *testing.T) {
	err := listenerEnds(t, &net.OpError{Op: "accept", Net: "tcp", Err: net.ErrClosed})
	if err == nil || err.Error() != "the ssh listener stopped" {
		t.Errorf("serve returned %v, want: the ssh listener stopped", err)
	}
}

// blockingListener is a listener whose Close does not return until
// release is closed.
type blockingListener struct {
	net.Listener
	release chan struct{}
}

func (l *blockingListener) Close() error {
	<-l.release
	return l.Listener.Close()
}

// A stop that cannot finish is given up on after stopTimeout: serve
// returns, and the log says what it was still waiting for.
func TestServeStopIsBounded(t *testing.T) {
	cfg := startAPIServer(t)
	alice := newUser(t, "alice", false)
	o := envOptions(t, cfg, alice)
	bl := &blockingListener{release: make(chan struct{})}
	o.wrapTCP = func(l net.Listener) net.Listener {
		bl.Listener = l
		return bl
	}
	o.stopTimeout = 300 * time.Millisecond
	k := startKosh(t, o)
	t.Cleanup(func() { close(bl.release) })
	c := dialTCP(t, k, alice)
	c.waitFor(t, "Pods(default)")

	start := time.Now()
	k.cancel()
	select {
	case err := <-k.done:
		if err != nil {
			t.Errorf("serve returned %v after a stop that was asked for", err)
		}
	case <-time.After(o.stopTimeout + time.Second):
		t.Fatalf("serve has not returned %v after its context was cancelled", o.stopTimeout+time.Second)
	}
	t.Logf("returned in %v", time.Since(start))
	k.done <- nil // for the cleanup

	var warned *record
	for _, r := range k.log.records() {
		if r.Level == slog.LevelWarn && r.Msg == "kosh did not stop in time" {
			warned = &r
		}
	}
	if warned == nil || !strings.Contains(warned.Attrs["waiting_for"], "the SSH server") {
		t.Errorf("no warning that names the SSH server:\n%s", k.log.dump())
	}
}

// What Go's standard log package prints, as quic-go prints a warning
// about the UDP receive buffer, is a record of kosh's log.
func TestStandardLogGoesToTheLog(t *testing.T) {
	out, flags := stdlog.Writer(), stdlog.Flags()
	t.Cleanup(func() {
		stdlog.SetOutput(out)
		stdlog.SetFlags(flags)
		// The next serve routes the standard log again.
		stdlogOnce = sync.Once{}
	})
	cfg := startAPIServer(t)
	alice := newUser(t, "alice", false)
	k := startKosh(t, envOptions(t, cfg, alice))

	stdlog.Print("marker line")
	var got []record
	for _, r := range k.log.records() {
		if strings.Contains(r.Msg, "marker line") {
			got = append(got, r)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d records of the line, want one:\n%s", len(got), k.log.dump())
	}
	if r := got[0]; r.Level != slog.LevelWarn || r.Msg != "marker line" || r.Attrs["source"] != "log" {
		t.Errorf("%v, want the line at warn level with source=log", r)
	}
}

// A line the standard logger carries, which may repeat what a peer sent
// (the SSH library's errors come through Go's default slog), is cut to
// 1 KiB at a rune boundary.
func TestStandardLogLineIsCut(t *testing.T) {
	var rec recorder
	was := stdlogTo.Swap(rec.logger())
	t.Cleanup(func() { stdlogTo.Store(was) })
	line := strings.Repeat("é", 1000) + "\n" // 2000 bytes
	if n, err := (stdlogWriter{}).Write([]byte(line)); n != len(line) || err != nil {
		t.Fatalf("Write: %d, %v", n, err)
	}
	if _, err := (stdlogWriter{}).Write([]byte("short\n")); err != nil {
		t.Fatal(err)
	}
	recs := rec.records()
	if len(recs) != 2 {
		t.Fatalf("records %v", recs)
	}
	msg := recs[0].Msg
	if len(msg) > 1024+len("…") || !strings.HasSuffix(msg, "…") || !utf8.ValidString(msg) ||
		!strings.HasPrefix(line, strings.TrimSuffix(msg, "…")) {
		t.Errorf("a line of 2000 bytes became %d bytes: %.80q…", len(msg), msg)
	}
	if recs[1].Msg != "short" {
		t.Errorf("a short line became %q", recs[1].Msg)
	}
}

// A kosh with iroh on, stopped as it is asked to, warns of nothing.
func TestServeStopIsQuiet(t *testing.T) {
	cfg := startAPIServer(t)
	alice := newUser(t, "alice", false)
	o := envOptions(t, cfg, alice)
	o.iroh = true
	o.irohLocal = true
	k := startKosh(t, o)
	if k.serving.ssh == nil || k.serving.iroh == nil {
		t.Fatalf("serving %+v, want TCP and iroh", k.serving)
	}
	// A session on each transport, which the stop ends.
	dialTCP(t, k, alice).waitFor(t, "Pods(default)")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := irohnet.DialLocal(ctx, k.serving.iroh.ID(), k.serving.iroh.LocalAddrs())
	if err != nil {
		t.Fatal(err)
	}
	c, err := connect(k, conn, alice)
	if err != nil {
		t.Fatalf("connecting over iroh: %v\nlog:\n%s", err, k.log.dump())
	}
	defer func() { _ = c.ssh.Close() }()
	c.waitFor(t, "Pods(default)")

	k.cancel()
	select {
	case err := <-k.done:
		if err != nil {
			t.Errorf("serve returned %v after a stop that was asked for", err)
		}
	case <-time.After(stopTimeout):
		t.Fatalf("serve has not returned %v after its context was cancelled", stopTimeout)
	}
	k.done <- nil // for the cleanup

	if _, ok := k.log.find("kosh has stopped"); !ok {
		t.Errorf("no line kosh has stopped:\n%s", k.log.dump())
	}
	for _, r := range k.log.records() {
		if r.Level >= slog.LevelWarn {
			t.Errorf("a clean stop logged %v", r)
		}
	}
}
