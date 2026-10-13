package kube

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"

	"github.com/fables-for-robots/kosh/render"
)

// rediscoverAfter is how long a discovery that Resolve asked for keeps
// another one from happening.
const rediscoverAfter = 5 * time.Second

// discover reads the API's resources: those it prefers, less
// subresources and those that cannot be listed, with the GVR of the
// renderer where the API serves it. A group that fails is logged and
// left out; any other failure is an error.
func discover(ctx context.Context, disc discovery.DiscoveryInterface, renderers func(schema.GroupVersionResource) (render.Renderer, bool), log *slog.Logger) ([]Resource, error) {
	d := discovery.ToDiscoveryInterfaceWithContext(disc)
	preferred, err := discovery.ServerPreferredResourcesWithContext(ctx, d)
	if err != nil {
		if !discovery.IsGroupDiscoveryFailedError(err) {
			return nil, fmt.Errorf("reading the API's resources: %w", err)
		}
		log.Warn("some groups of the API could not be discovered", "err", err)
	}
	_, all, err := discovery.ServerGroupsAndResourcesWithContext(ctx, d)
	if err != nil && !discovery.IsGroupDiscoveryFailedError(err) {
		return nil, fmt.Errorf("reading the API's resources: %w", err)
	}

	// Every version the API serves of a group and resource, in the order
	// the API gives them.
	served := map[schema.GroupResource][]schema.GroupVersion{}
	apiResources := map[schema.GroupVersionResource]metav1.APIResource{}
	for _, list := range all {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, ar := range list.APIResources {
			if strings.Contains(ar.Name, "/") {
				continue
			}
			gr := schema.GroupResource{Group: gv.Group, Resource: ar.Name}
			served[gr] = append(served[gr], gv)
			apiResources[gv.WithResource(ar.Name)] = ar
		}
	}

	out := []Resource{}
	for _, list := range preferred {
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			log.Warn("the API names a group version that does not parse", "groupVersion", list.GroupVersion, "err", err)
			continue
		}
		for _, ar := range list.APIResources {
			if strings.Contains(ar.Name, "/") || !hasVerb(ar.Verbs, "list") {
				continue
			}
			r := resourceOf(gv.WithResource(ar.Name), ar)
			for _, v := range append([]schema.GroupVersion{gv}, served[r.GVR.GroupResource()]...) {
				gvr := v.WithResource(ar.Name)
				if _, ok := renderers(gvr); ok {
					r = resourceOf(gvr, apiResources[gvr])
					r.Typed = true
					break
				}
			}
			out = append(out, r)
		}
	}
	sortResources(out)
	return out, nil
}

func resourceOf(gvr schema.GroupVersionResource, ar metav1.APIResource) Resource {
	return Resource{
		GVR:        gvr,
		Kind:       ar.Kind,
		Singular:   ar.SingularName,
		ShortNames: append([]string(nil), ar.ShortNames...),
		Namespaced: ar.Namespaced,
		Verbs:      append([]string(nil), ar.Verbs...),
	}
}

func hasVerb(verbs metav1.Verbs, verb string) bool {
	for _, v := range verbs {
		if v == verb {
			return true
		}
	}
	return false
}

func sortResources(rr []Resource) {
	sort.SliceStable(rr, func(i, j int) bool {
		if rr[i].GVR.Resource != rr[j].GVR.Resource {
			return rr[i].GVR.Resource < rr[j].GVR.Resource
		}
		return rr[i].GVR.Group < rr[j].GVR.Group
	})
}

// refresh discovers the resources again and keeps them. A discovery
// that fails keeps what was known, and is logged.
func (c *Cluster) refresh(ctx context.Context) {
	rr, err := discover(ctx, c.disc, c.opts.Renderers, c.log)
	if err != nil {
		if ctx.Err() == nil {
			c.log.Warn("discovery failed; keeping the resources known so far", "err", err)
		}
		return
	}
	c.setResources(rr)
}

func (c *Cluster) setResources(rr []Resource) {
	c.resMu.Lock()
	c.resources = rr
	c.resMu.Unlock()
}

// discoverEvery runs the discovery every opts.DiscoveryEvery until the
// Cluster is closed.
func (c *Cluster) discoverEvery() {
	defer c.wg.Done()
	t := time.NewTicker(c.opts.DiscoveryEvery)
	defer t.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-t.C:
			c.refresh(c.ctx)
		}
	}
}

// resourceList returns every resource of the cluster that can be
// listed, sorted by name, then group.
func (c *Cluster) resourceList() []Resource {
	c.resMu.RLock()
	defer c.resMu.RUnlock()
	return append([]Resource(nil), c.resources...)
}

// resourceFor returns the Resource of a kind in a group, as discovery
// knows it: resourceFor("apps", "Deployment").
func (c *Cluster) resourceFor(group, kind string) (Resource, bool) {
	c.resMu.RLock()
	defer c.resMu.RUnlock()
	for _, r := range c.resources {
		if r.GVR.Group == group && r.Kind == kind {
			return r, true
		}
	}
	return Resource{}, false
}

// resolve returns the resource that answers to name. A name that nothing
// answers to is discovered again, at most once in rediscoverAfter for
// the whole Cluster, and looked up again.
func (c *Cluster) resolve(ctx context.Context, name string) (Resource, error) {
	if r, ok := c.lookup(name); ok {
		return r, nil
	}
	if c.disc != nil && c.mayRediscover() {
		c.refresh(ctx)
		if r, ok := c.lookup(name); ok {
			return r, nil
		}
	}
	return Resource{}, fmt.Errorf("%q: %w", name, ErrUnknownResource)
}

// mayRediscover reports whether Resolve may discover again now, and
// when it may, keeps others from doing it for rediscoverAfter.
func (c *Cluster) mayRediscover() bool {
	c.missMu.Lock()
	defer c.missMu.Unlock()
	if !c.lastMiss.IsZero() && time.Since(c.lastMiss) < rediscoverAfter {
		return false
	}
	c.lastMiss = time.Now()
	return true
}

// lookup finds the resource that answers to name, without regard to
// case: its plural name, its singular name, a short name, its kind, or
// plural.group. Of several, the typed one wins, then the one of the core
// group, then the first by group name.
func (c *Cluster) lookup(name string) (Resource, bool) {
	want := strings.ToLower(name)
	c.resMu.RLock()
	defer c.resMu.RUnlock()
	var best Resource
	found := false
	for _, r := range c.resources {
		if !answersTo(r, want) {
			continue
		}
		if !found || better(r, best) {
			best, found = r, true
		}
	}
	return best, found
}

func answersTo(r Resource, name string) bool {
	if name == "" {
		return false
	}
	plural := strings.ToLower(r.GVR.Resource)
	if name == plural || name == strings.ToLower(r.Singular) || name == strings.ToLower(r.Kind) {
		return true
	}
	if r.GVR.Group != "" && name == plural+"."+strings.ToLower(r.GVR.Group) {
		return true
	}
	for _, s := range r.ShortNames {
		if name == strings.ToLower(s) {
			return true
		}
	}
	return false
}

// better reports whether a is preferred to b for one name.
func better(a, b Resource) bool {
	if a.Typed != b.Typed {
		return a.Typed
	}
	if (a.GVR.Group == "") != (b.GVR.Group == "") {
		return a.GVR.Group == ""
	}
	return a.GVR.Group < b.GVR.Group
}
