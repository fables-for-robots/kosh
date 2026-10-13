package kube

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKubeconfig writes a kubeconfig of the given clusters and
// contexts, with current as its current context, and returns its path.
func writeKubeconfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"+body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRESTConfigList(t *testing.T) {
	// The first file has the user, the second the cluster, the context
	// and the current context: kubectl merges them.
	users := writeKubeconfig(t, `users:
- name: me
  user: {token: secret}
`)
	rest := writeKubeconfig(t, `clusters:
- name: there
  cluster: {server: "https://there.example:6443"}
contexts:
- name: there
  context: {cluster: there, user: me}
current-context: there
`)
	cfg, err := RESTConfig(users + string(filepath.ListSeparator) + rest)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://there.example:6443" || cfg.BearerToken != "secret" {
		t.Errorf("host %q, token %q: not the merged files'", cfg.Host, cfg.BearerToken)
	}

	// A file of the list that is not there is skipped.
	missing := filepath.Join(t.TempDir(), "missing")
	whole := writeKubeconfig(t, `clusters:
- name: here
  cluster: {server: "https://here.example:6443"}
users:
- name: me
  user: {token: secret}
contexts:
- name: here
  context: {cluster: here, user: me}
current-context: here
`)
	cfg, err = RESTConfig(missing + string(filepath.ListSeparator) + whole)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://here.example:6443" {
		t.Errorf("host %q: not the second file's", cfg.Host)
	}

	// Neither is there: an error that names the value.
	both := missing + string(filepath.ListSeparator) + filepath.Join(t.TempDir(), "missing too")
	if _, err := RESTConfig(both); err == nil || !strings.Contains(err.Error(), both) {
		t.Errorf("two files that are not there: %v, want an error that names %q", err, both)
	}
}

func TestEnvVersion(t *testing.T) {
	cfg := startAPIServer(t)
	c := newEnvCluster(t, cfg, Options{})
	if v := c.Version(); !strings.HasPrefix(v, "v1.") || v != c.Session(context.Background(), fullGrant).ClusterVersion() {
		t.Errorf("Version %q", v)
	}
}
