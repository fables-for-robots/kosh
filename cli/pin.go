//go:build pin

// This file is never built into kosh. It names the packages the tasks of
// the stage 1 plan use, so that go.mod holds their versions before the
// code that imports them is written, and tasks that are built side by
// side do not each change go.mod. The plan's last task removes it.
package cli

import (
	_ "charm.land/bubbles/v2/textarea"
	_ "charm.land/bubbles/v2/textinput"
	_ "charm.land/lipgloss/v2"
	_ "charm.land/ssh"
	_ "charm.land/wish/v2"
	_ "charm.land/wish/v2/bubbletea"
	_ "github.com/charmbracelet/x/ansi"
	_ "github.com/tmc/go-iroh/iroh"
	_ "github.com/tmc/go-iroh/iroh/mdns"
	_ "golang.org/x/crypto/ssh"
	_ "k8s.io/api/core/v1"
	_ "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	_ "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	_ "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	_ "k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/dynamic/fake"
	_ "k8s.io/client-go/informers"
	_ "k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/kubernetes/fake"
	_ "k8s.io/client-go/tools/clientcmd"
	_ "k8s.io/kubectl/pkg/describe"
	_ "k8s.io/kubectl/pkg/drain"
	_ "k8s.io/kubectl/pkg/polymorphichelpers"
	_ "sigs.k8s.io/controller-runtime/pkg/envtest"
	_ "sigs.k8s.io/yaml"
)
