package cli

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	ucli "github.com/urfave/cli/v2"
)

// serveOptions are the settings of kosh serve: its flags, and what
// tests set besides them.
type serveOptions struct {
	sshListen      string // TCP address of the SSH server; empty for none
	iroh           bool   // accept SSH over iroh
	irohBind       string // UDP address of the endpoint; empty for every address, any port
	irohRelay      bool   // the default relays and the pkarr record
	irohMDNS       bool   // mDNS
	stateDir       string // the two keys
	authorizedKeys string // the key list
	kubeconfig     string // empty for the pod's ServiceAccount
}

// serveCommand is kosh serve, which runs runServe with its flags.
func serveCommand(runServe func(context.Context, serveOptions) error) *ucli.Command {
	return &ucli.Command{
		Name:  "serve",
		Usage: "serve the terminal UI over SSH and iroh",
		Flags: []ucli.Flag{
			&ucli.StringFlag{Name: "ssh-listen", EnvVars: []string{"KOSH_SSH_LISTEN"}, Value: ":2222",
				Usage: "TCP address of the SSH server; empty for none"},
			&ucli.BoolFlag{Name: "iroh", EnvVars: []string{"KOSH_IROH"}, Value: true,
				Usage: "accept SSH over iroh"},
			&ucli.StringFlag{Name: "iroh-bind", EnvVars: []string{"KOSH_IROH_BIND"},
				Usage: "UDP address of the iroh endpoint (default: every address, any port)"},
			&ucli.BoolFlag{Name: "iroh-relay", EnvVars: []string{"KOSH_IROH_RELAY"}, Value: true,
				Usage: "use the default relays and publish a pkarr record with number0's DNS"},
			&ucli.BoolFlag{Name: "iroh-mdns", EnvVars: []string{"KOSH_IROH_MDNS"}, Value: true,
				Usage: "announce the iroh endpoint on the local network"},
			&ucli.StringFlag{Name: "state-dir", EnvVars: []string{"KOSH_STATE_DIR"}, Value: "/var/lib/kosh",
				Usage: "the directory of the iroh key and the SSH host key"},
			&ucli.StringFlag{Name: "authorized-keys", EnvVars: []string{"KOSH_AUTHORIZED_KEYS"}, Value: "/etc/kosh/authorized_keys",
				Usage: "the key list"},
			&ucli.StringFlag{Name: "kubeconfig", EnvVars: []string{"KUBECONFIG"},
				Usage: "a kubeconfig instead of the pod's ServiceAccount, for running kosh outside a cluster"},
		},
		Action: func(c *ucli.Context) error {
			return runServe(c.Context, serveOptions{
				sshListen:      c.String("ssh-listen"),
				iroh:           c.Bool("iroh"),
				irohBind:       c.String("iroh-bind"),
				irohRelay:      c.Bool("iroh-relay"),
				irohMDNS:       c.Bool("iroh-mdns"),
				stateDir:       c.String("state-dir"),
				authorizedKeys: c.String("authorized-keys"),
				kubeconfig:     c.String("kubeconfig"),
			})
		},
	}
}

// check refuses settings with which kosh cannot serve anybody, before
// anything is started.
func (o serveOptions) check() (bind netip.AddrPort, err error) {
	if o.sshListen == "" && !o.iroh {
		return bind, errors.New("kosh serve: both transports are off: set --ssh-listen or --iroh")
	}
	if o.iroh && !o.irohRelay && !o.irohMDNS {
		return bind, errors.New("kosh serve: iroh is on and nobody could find it: set --iroh-relay or --iroh-mdns")
	}
	if o.irohBind != "" {
		if bind, err = netip.ParseAddrPort(o.irohBind); err != nil {
			return bind, fmt.Errorf("kosh serve: --iroh-bind %q is not an address and port: %w", o.irohBind, err)
		}
	}
	return bind, nil
}

// serve is kosh serve.
func serve(ctx context.Context, o serveOptions) error {
	if _, err := o.check(); err != nil {
		return err
	}
	return nil
}
