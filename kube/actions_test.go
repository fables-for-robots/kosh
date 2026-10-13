package kube

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/fables-for-robots/kosh/keys"
)

// actionFixture is a Cluster on fakes that hold the same objects, a
// session of it, and the records of its log.
type actionFixture struct {
	*env
	s   *session
	log *auditLog
}

// actionEnv makes a Cluster on fakes that hold objs, with a log that
// keeps its records, and a session with grant. before runs before the
// Cluster is made, for reactors. What the Cluster sent while it was
// made is forgotten, so that the fakes' actions are the session's.
func actionEnv(t *testing.T, grant keys.Grant, before func(*env), objs ...runtime.Object) *actionFixture {
	t.Helper()
	log := newAuditLog()
	e := newEnv(t, envConfig{opts: Options{Log: log.logger()}, before: before}, objs...)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f := &actionFixture{env: e, s: e.c.Session(ctx, grant).(*session), log: log}
	f.forget()
	return f
}

// forget clears what the fakes recorded and what was logged.
func (f *actionFixture) forget() {
	f.typed.ClearActions()
	f.dyn.ClearActions()
	f.log.reset()
}

// sent returns what the session sent to either fake.
func (f *actionFixture) sent() []k8stesting.Action {
	return append(f.typed.Actions(), f.dyn.Actions()...)
}

// ref returns the ObjectRef of an object of the resource group/plural.
func (f *actionFixture) ref(t *testing.T, group, plural, namespace, name string) ObjectRef {
	t.Helper()
	return ObjectRef{Resource: f.resource(t, group, plural), Namespace: namespace, Name: name}
}
