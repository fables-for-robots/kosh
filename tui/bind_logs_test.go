package tui

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/fables-for-robots/kosh/keys"
	"github.com/fables-for-robots/kosh/kube"
	"github.com/fables-for-robots/kosh/kube/kubetest"
	"github.com/fables-for-robots/kosh/render"
)

// logKeyOf reports whether a table of res has key for grant.
func logKeyOf(res kube.Resource, grant keys.Grant, key string) bool {
	return slices.ContainsFunc(bindingsFor(res, grant, canAll), func(b binding) bool { return b.Key == key })
}

// logContainers returns a session in the containers of web-1, with the
// container app.
func logContainers(t *testing.T) (*kubetest.Session, *harness) {
	t.Helper()
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	sess.SetTable(queryOf(kube.Containers, logPod), kube.Table{
		Columns: []render.Column{{Name: "NAME"}, {Name: "IMAGE"}},
		Rows:    []render.Row{{Name: "app", Cells: []string{"app", "nginx"}}},
	})
	hs := newHarness(t, sess, 100, 30)
	hs.press("enter")
	if hs.title() != "Containers(pods/default/web-1)[1]" {
		t.Fatalf("the title is %q", hs.title())
	}
	return sess, hs
}

func TestLogsOpen(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("l")
	ref, opts := logLastOpts(t, sess)
	if ref.String() != logPod.String() || ref.Resource.GVR != logPod.Resource.GVR || opts != (kube.LogOptions{From: kube.LogTail}) {
		t.Errorf("l called Logs(%s, %+v)", ref, opts)
	}
	v, ok := hs.root().top().(*logView)
	if !ok {
		t.Fatalf("l did not open the log: %T", hs.root().top())
	}
	sess.LogOf(logPod).Send(logTexts(1, 3)...)
	hs.await("three lines", func() bool { return logHas(v, "line 0003") })
	if hs.title() != "Logs(pods/default/web-1) [autoscroll:on] tail" {
		t.Errorf("the title is %q", hs.title())
	}
	if l := hs.lines()[len(hs.lines())-2]; words(l) != "<pods> <logs>" {
		t.Errorf("the crumbs are %q", l)
	}
	hs.golden()
}

func TestLogsOfAContainer(t *testing.T) {
	sess, hs := logContainers(t)
	hs.press("l")
	ref, opts := logLastOpts(t, sess)
	if ref.String() != logPod.String() || ref.Resource.GVR != logPod.Resource.GVR || opts.Container != "app" || opts.Previous {
		t.Errorf("l called Logs(%s, %+v)", ref, opts)
	}
	if !strings.HasPrefix(hs.title(), "Logs(pods/default/web-1:app) ") {
		t.Errorf("the title is %q", hs.title())
	}
	// enter on a container opens its log too.
	hs.press("esc", "enter")
	if !strings.HasPrefix(hs.title(), "Logs(pods/default/web-1:app) ") {
		t.Errorf("after enter the title is %q", hs.title())
	}
}

func TestLogsPrevious(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	hs := newHarness(t, sess, 100, 30)
	hs.press("p")
	if _, opts := logLastOpts(t, sess); !opts.Previous || opts.Container != "" {
		t.Errorf("p opened %+v", opts)
	}
	if !strings.HasPrefix(hs.title(), "Logs(pods/default/web-1) previous ") {
		t.Errorf("the title is %q", hs.title())
	}
	sess, hs = logContainers(t)
	hs.press("p")
	if _, opts := logLastOpts(t, sess); !opts.Previous || opts.Container != "app" {
		t.Errorf("p in the containers opened %+v", opts)
	}
	if !strings.HasPrefix(hs.title(), "Logs(pods/default/web-1:app) previous ") {
		t.Errorf("the title is %q", hs.title())
	}
	full := keys.Grant{Name: "alice"}
	if logKeyOf(kubetest.Resource("deployments"), full, "p") {
		t.Error("a table of deployments has p")
	}
}

func TestLogKeysApply(t *testing.T) {
	kinds := []kube.Resource{kube.Containers}
	for _, plural := range []string{"pods", "deployments", "replicasets", "statefulsets", "daemonsets", "jobs", "services"} {
		kinds = append(kinds, kubetest.Resource(plural))
	}
	for _, g := range []keys.Grant{{Name: "alice", Role: clusterAdmin}, viewGrant} {
		for _, res := range kinds {
			if !logKeyOf(res, g, "l") {
				t.Errorf("a table of %s has not got l (%s)", res.GVR.Resource, g.Role)
			}
			isPod := res.Is("", "pods") || res.GVR == kube.Containers.GVR
			if logKeyOf(res, g, "p") != isPod {
				t.Errorf("a table of %s has p: %v (%s)", res.GVR.Resource, !isPod, g.Role)
			}
		}
		for _, plural := range []string{"configmaps", "cronjobs", "nodes"} {
			if logKeyOf(kubetest.Resource(plural), g, "l") || logKeyOf(kubetest.Resource(plural), g, "p") {
				t.Errorf("a table of %s has a log key", plural)
			}
		}
	}
	for _, b := range bindingsFor(kubetest.Resource("pods"), keys.Grant{}, canAll) {
		switch b.Key {
		case "l":
			if b.Hint != (hint{"l", "Logs"}) || b.Order != 20 {
				t.Errorf("l is %+v", b)
			}
		case "p":
			if b.Hint != (hint{"p", "Logs Previous"}) || b.Order != 21 {
				t.Errorf("p is %+v", b)
			}
		}
	}
}

func TestLogOpenErrorIsAFlash(t *testing.T) {
	sess := newSession()
	sess.SetTable(query("pods", "default"), podsNamed("web-1"))
	sess.Errs["Logs"] = errors.New("pods \"web-1\" is forbidden")
	hs := newHarness(t, sess, 100, 30)
	hs.press("l")
	if _, ok := hs.root().top().(*logView); ok {
		t.Error("the log view was pushed")
	}
	if l := hs.drawnLine(`pods "web-1" is forbidden`); !strings.Contains(l, sgr(styleError)) {
		t.Errorf("the flash is %q", l)
	}
}
