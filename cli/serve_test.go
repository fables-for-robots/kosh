package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/klog/v2"
)

// serveVars are the environment variables kosh serve reads.
var serveVars = []string{
	"KOSH_SSH_LISTEN", "KOSH_IROH", "KOSH_IROH_BIND", "KOSH_IROH_RELAY", "KOSH_IROH_MDNS",
	"KOSH_STATE_DIR", "KOSH_AUTHORIZED_KEYS", "KUBECONFIG",
}

// defaultServe is what kosh serve runs with when nothing is set.
var defaultServe = serveOptions{
	sshListen:      ":2222",
	iroh:           true,
	irohBind:       "",
	irohRelay:      true,
	irohMDNS:       true,
	stateDir:       "/var/lib/kosh",
	authorizedKeys: "/etc/kosh/authorized_keys",
	kubeconfig:     "",
}

func TestServeFlagsAndEnvironment(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want func(*serveOptions)
	}{
		{"defaults", nil, nil, func(*serveOptions) {}},

		{"ssh-listen flag", []string{"--ssh-listen", "127.0.0.1:22"}, nil, func(o *serveOptions) { o.sshListen = "127.0.0.1:22" }},
		{"ssh-listen variable", nil, map[string]string{"KOSH_SSH_LISTEN": "127.0.0.1:22"}, func(o *serveOptions) { o.sshListen = "127.0.0.1:22" }},
		{"ssh-listen empty flag", []string{"--ssh-listen", ""}, nil, func(o *serveOptions) { o.sshListen = "" }},
		{"ssh-listen empty variable", nil, map[string]string{"KOSH_SSH_LISTEN": ""}, func(o *serveOptions) { o.sshListen = "" }},

		{"iroh flag", []string{"--iroh=false"}, nil, func(o *serveOptions) { o.iroh = false }},
		{"iroh variable", nil, map[string]string{"KOSH_IROH": "false"}, func(o *serveOptions) { o.iroh = false }},

		{"iroh-bind flag", []string{"--iroh-bind", "0.0.0.0:4433"}, nil, func(o *serveOptions) { o.irohBind = "0.0.0.0:4433" }},
		{"iroh-bind variable", nil, map[string]string{"KOSH_IROH_BIND": "0.0.0.0:4433"}, func(o *serveOptions) { o.irohBind = "0.0.0.0:4433" }},

		{"iroh-relay flag", []string{"--iroh-relay=false"}, nil, func(o *serveOptions) { o.irohRelay = false }},
		{"iroh-relay variable", nil, map[string]string{"KOSH_IROH_RELAY": "0"}, func(o *serveOptions) { o.irohRelay = false }},

		{"iroh-mdns flag", []string{"--iroh-mdns=false"}, nil, func(o *serveOptions) { o.irohMDNS = false }},
		{"iroh-mdns variable", nil, map[string]string{"KOSH_IROH_MDNS": "false"}, func(o *serveOptions) { o.irohMDNS = false }},

		{"state-dir flag", []string{"--state-dir", "/s"}, nil, func(o *serveOptions) { o.stateDir = "/s" }},
		{"state-dir variable", nil, map[string]string{"KOSH_STATE_DIR": "/s"}, func(o *serveOptions) { o.stateDir = "/s" }},

		{"authorized-keys flag", []string{"--authorized-keys", "/k"}, nil, func(o *serveOptions) { o.authorizedKeys = "/k" }},
		{"authorized-keys variable", nil, map[string]string{"KOSH_AUTHORIZED_KEYS": "/k"}, func(o *serveOptions) { o.authorizedKeys = "/k" }},

		{"kubeconfig flag", []string{"--kubeconfig", "/c"}, nil, func(o *serveOptions) { o.kubeconfig = "/c" }},
		{"kubeconfig variable", nil, map[string]string{"KUBECONFIG": "/c"}, func(o *serveOptions) { o.kubeconfig = "/c" }},

		{"the command line wins",
			[]string{"--ssh-listen", ":1", "--iroh=true", "--iroh-bind", "127.0.0.1:1", "--iroh-relay=true", "--iroh-mdns=true",
				"--state-dir", "/flag/s", "--authorized-keys", "/flag/k", "--kubeconfig", "/flag/c"},
			map[string]string{
				"KOSH_SSH_LISTEN": ":2", "KOSH_IROH": "false", "KOSH_IROH_BIND": "127.0.0.1:2", "KOSH_IROH_RELAY": "false",
				"KOSH_IROH_MDNS": "false", "KOSH_STATE_DIR": "/env/s", "KOSH_AUTHORIZED_KEYS": "/env/k", "KUBECONFIG": "/env/c",
			},
			func(o *serveOptions) {
				o.sshListen, o.irohBind = ":1", "127.0.0.1:1"
				o.stateDir, o.authorizedKeys, o.kubeconfig = "/flag/s", "/flag/k", "/flag/c"
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			unsetenv(t, serveVars...)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var got *serveOptions
			app := newApp(func(_ context.Context, o serveOptions) error {
				got = &o
				return nil
			})
			if _, _, err := run(t, app, append([]string{"serve"}, tt.args...)...); err != nil {
				t.Fatal(err)
			}
			if got == nil {
				t.Fatal("serve did not run")
			}
			want := defaultServe
			tt.want(&want)
			if !reflect.DeepEqual(*got, want) {
				t.Errorf("got  %+v\nwant %+v", *got, want)
			}
		})
	}
}

// refused runs kosh serve with args after a state directory that does
// not exist, and returns its error. It fails the test when serve made
// the directory: a refusal starts nothing.
func refused(t *testing.T, args ...string) error {
	t.Helper()
	unsetenv(t, serveVars...)
	dir := t.TempDir() + "/state"
	_, _, err := run(t, App(), append([]string{"serve", "--state-dir", dir, "--authorized-keys", dir + "/keys"}, args...)...)
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Errorf("the state directory was made: %v", statErr)
	}
	return err
}

func TestServeRefusesBothTransportsOff(t *testing.T) {
	err := refused(t, "--ssh-listen", "", "--iroh=false")
	if err == nil || err.Error() != "both transports are off: set --ssh-listen or --iroh" {
		t.Errorf("error %v", err)
	}
}

func TestServeRefusesIrohNobodyCanFind(t *testing.T) {
	err := refused(t, "--iroh-relay=false", "--iroh-mdns=false")
	if err == nil || err.Error() != "iroh is on and nobody could find it: set --iroh-relay or --iroh-mdns" {
		t.Errorf("error %v", err)
	}
}

func TestServeRefusesBadBind(t *testing.T) {
	for _, bind := range []string{"nonsense", "127.0.0.1", ":4433", "localhost:4433"} {
		err := refused(t, "--iroh-bind", bind)
		if err == nil || !strings.HasPrefix(err.Error(), "--iroh-bind ") || !strings.Contains(err.Error(), `"`+bind+`"`) {
			t.Errorf("--iroh-bind %s: error %v, want one that names the flag and the value", bind, err)
		}
	}
}

// recorder keeps what a logger it made was given, for the tests to look
// at.
type recorder struct {
	mu   sync.Mutex
	recs []record
}

type record struct {
	Level slog.Level
	Msg   string
	Attrs map[string]string
}

func (r record) String() string { return fmt.Sprintf("%s %q %v", r.Level, r.Msg, r.Attrs) }

// logger returns a logger that records everything, debug included.
func (r *recorder) logger() *slog.Logger { return slog.New(recordingHandler{r: r}) }

func (r *recorder) records() []record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]record(nil), r.recs...)
}

// find returns the first record with the message msg.
func (r *recorder) find(msg string) (record, bool) {
	for _, rec := range r.records() {
		if rec.Msg == msg {
			return rec, true
		}
	}
	return record{}, false
}

func (r *recorder) dump() string {
	var b strings.Builder
	for _, rec := range r.records() {
		b.WriteString(rec.String())
		b.WriteByte('\n')
	}
	return b.String()
}

type recordingHandler struct {
	r     *recorder
	attrs []slog.Attr
}

func (h recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h recordingHandler) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]string{}
	for _, a := range h.attrs {
		attrs[a.Key] = a.Value.String()
	}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.r.mu.Lock()
	h.r.recs = append(h.r.recs, record{Level: rec.Level, Msg: rec.Message, Attrs: attrs})
	h.r.mu.Unlock()
	return nil
}

func (h recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return recordingHandler{r: h.r, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h recordingHandler) WithGroup(string) slog.Handler { return h }

// waitTime is how long a test waits for something that should happen
// soon. It is a deadline, not a sleep.
const waitTime = 5 * time.Second

// eventually waits until cond holds, and fails the test when it does not
// within d.
func eventually(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v: %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// noGoroutinesLeft waits until there are no more goroutines than before,
// and fails the test with every goroutine's stack when there are.
func noGoroutinesLeft(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(waitTime)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			n := runtime.Stack(buf, true)
			t.Fatalf("%d goroutines, %d before:\n%s", runtime.NumGoroutine(), before, buf[:n])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// freePort returns a TCP address on the loopback address that nothing
// listens on.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

// portIsFree fails the test when addr cannot be bound.
func portIsFree(t *testing.T, addr string) {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("the port %s is not free: %v", addr, err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
}

// keyListFile writes a key list of one new key and returns its path.
func keyListFile(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "authorized_keys")
	writeFile(t, path, string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	return path
}

// unreachableKubeconfig writes a kubeconfig whose API server nobody
// answers for, and returns its path.
func unreachableKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	writeFile(t, path, `apiVersion: v1
kind: Config
clusters:
- name: nowhere
  cluster: {server: "https://127.0.0.1:1", insecure-skip-tls-verify: true}
users:
- name: me
  user: {token: nothing}
contexts:
- name: nowhere
  context: {cluster: nowhere, user: me}
current-context: nowhere
`)
	return path
}

func TestServeFailsOnBadStateDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "iroh.key"), "not a key\n")
	err := serve(context.Background(), serveOptions{
		sshListen: freePort(t), stateDir: dir, authorizedKeys: keyListFile(t),
		kubeconfig: unreachableKubeconfig(t), log: (&recorder{}).logger(),
	})
	if err == nil || !strings.HasPrefix(err.Error(), "the state directory could not be loaded") ||
		!strings.Contains(err.Error(), dir) {
		t.Errorf("error %v, want one that says the state directory could not be loaded", err)
	}
}

func TestServeFailsOnMissingKeyList(t *testing.T) {
	before := runtime.NumGoroutine()
	path := filepath.Join(t.TempDir(), "authorized_keys")
	err := serve(context.Background(), serveOptions{
		sshListen: freePort(t), stateDir: t.TempDir(), authorizedKeys: path,
		kubeconfig: unreachableKubeconfig(t), log: (&recorder{}).logger(),
	})
	if err == nil || !strings.HasPrefix(err.Error(), "the key list could not be read") ||
		!strings.Contains(err.Error(), path) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("error %v, want one that says the key list could not be read and names %s", err, path)
	}
	noGoroutinesLeft(t, before)
}

func TestServeFailsOnMissingKubeconfig(t *testing.T) {
	before := runtime.NumGoroutine()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	err := serve(context.Background(), serveOptions{
		sshListen: freePort(t), stateDir: t.TempDir(), authorizedKeys: keyListFile(t),
		kubeconfig: path, log: (&recorder{}).logger(),
	})
	if err == nil || !strings.HasPrefix(err.Error(), "the cluster's configuration could not be read") || !strings.Contains(err.Error(), path) {
		t.Errorf("error %v, want one that names %s", err, path)
	}
	noGoroutinesLeft(t, before)
}

func TestServeFailsOnUnreachableCluster(t *testing.T) {
	before := runtime.NumGoroutine()
	addr := freePort(t)
	start := time.Now()
	err := serve(context.Background(), serveOptions{
		sshListen: addr, stateDir: t.TempDir(), authorizedKeys: keyListFile(t),
		kubeconfig: unreachableKubeconfig(t), log: (&recorder{}).logger(),
	})
	if took := time.Since(start); took > clusterTimeout {
		t.Errorf("took %v", took)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "the cluster could not be reached") {
		t.Errorf("error %v, want one that says the cluster could not be reached", err)
	}
	portIsFree(t, addr)
	noGoroutinesLeft(t, before)
}

func TestKlogGoesToTheLogAtDebug(t *testing.T) {
	var rec recorder
	routeKlog(rec.logger())
	klog.InfoS("an informational line of klog", "k", "v")
	klog.ErrorS(errors.New("boom"), "an error of klog")
	klog.Background().WithValues("reflector", "pods").Error(errors.New("list failed"), "Failed to watch")
	klog.Flush()
	want := map[string]bool{"an informational line of klog": false, "an error of klog": false, "Failed to watch": false}
	for _, r := range rec.records() {
		if _, ok := want[r.Msg]; !ok {
			continue
		}
		want[r.Msg] = true
		if r.Level != slog.LevelDebug {
			t.Errorf("%v: not at debug level", r)
		}
	}
	for msg, seen := range want {
		if !seen {
			t.Errorf("%q is not in the log:\n%s", msg, rec.dump())
		}
	}
	if r, ok := rec.find("Failed to watch"); ok && r.Attrs["reflector"] != "pods" {
		t.Errorf("%v: the values of the logger are lost", r)
	}

	// At info level, as kosh logs, none of them is written.
	var buf bytes.Buffer
	routeKlog(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	klog.ErrorS(errors.New("boom"), "an error of klog")
	klog.Background().Error(errors.New("list failed"), "Failed to watch")
	klog.Flush()
	if buf.Len() > 0 {
		t.Errorf("written at info level: %s", buf.String())
	}
}

// What client-go logs at a verbosity above 0, request and response
// bodies among it, never reaches the log, even at debug level: a
// Secret's YAML and the text of an edit are such bodies.
func TestKlogVerboseLinesAreDropped(t *testing.T) {
	var buf bytes.Buffer
	routeKlog(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	ctx := klog.NewContext(context.Background(), klog.Background())
	klog.FromContext(ctx).V(2).Info("Request Body", "body", "SECRETVALUE")
	klog.Background().V(7).Info("Response Body", "body", "SECRETVALUE")
	klog.Background().V(1).Info("verbose", "body", "SECRETVALUE")
	klog.Background().Info("an informational line of klog")
	klog.Background().Error(errors.New("boom"), "an error of klog")
	klog.Flush()
	out := buf.String()
	if strings.Contains(out, "SECRETVALUE") {
		t.Errorf("a verbose line is in the log:\n%s", out)
	}
	for _, want := range []string{`level=DEBUG msg="an informational line of klog"`, `level=DEBUG msg="an error of klog"`} {
		if !strings.Contains(out, want) {
			t.Errorf("no %s in the log:\n%s", want, out)
		}
	}
}

// A stop asked for while the cluster is being reached is a stop: serve
// returns nil at once, and leaves nothing behind.
func TestServeStopDuringStart(t *testing.T) {
	// An API server that accepts connections and never answers.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		_ = l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	writeFile(t, kubeconfig, `apiVersion: v1
kind: Config
clusters:
- name: silent
  cluster: {server: "https://`+l.Addr().String()+`", insecure-skip-tls-verify: true}
users:
- name: me
  user: {token: nothing}
contexts:
- name: silent
  context: {cluster: silent, user: me}
current-context: silent
`)

	before := runtime.NumGoroutine()
	addr := freePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, serveOptions{
			sshListen: addr, stateDir: t.TempDir(), authorizedKeys: keyListFile(t),
			kubeconfig: kubeconfig, log: (&recorder{}).logger(),
		})
	}()
	select {
	case <-accepted:
	case err := <-done:
		t.Fatalf("serve returned %v before it reached the API server", err)
	case <-time.After(waitTime):
		t.Fatal("serve has not reached the API server")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve returned %v after a stop that was asked for", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve has not returned 2 seconds after its context was cancelled")
	}
	portIsFree(t, addr)
	// net/http goes on with a TLS handshake it began, for its pool,
	// after the request was cancelled; the API server that never
	// answers ends it here.
	mu.Lock()
	for _, c := range conns {
		_ = c.Close()
	}
	mu.Unlock()
	noGoroutinesLeft(t, before+1) // the API server's Accept
}
