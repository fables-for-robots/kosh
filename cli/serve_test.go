package cli

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
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
	if err == nil || err.Error() != "kosh serve: both transports are off: set --ssh-listen or --iroh" {
		t.Errorf("error %v", err)
	}
}

func TestServeRefusesIrohNobodyCanFind(t *testing.T) {
	err := refused(t, "--iroh-relay=false", "--iroh-mdns=false")
	if err == nil || err.Error() != "kosh serve: iroh is on and nobody could find it: set --iroh-relay or --iroh-mdns" {
		t.Errorf("error %v", err)
	}
}

func TestServeRefusesBadBind(t *testing.T) {
	for _, bind := range []string{"nonsense", "127.0.0.1", ":4433", "localhost:4433"} {
		err := refused(t, "--iroh-bind", bind)
		if err == nil || !strings.Contains(err.Error(), "--iroh-bind") || !strings.Contains(err.Error(), `"`+bind+`"`) {
			t.Errorf("--iroh-bind %s: error %v, want one that names the flag and the value", bind, err)
		}
	}
}
