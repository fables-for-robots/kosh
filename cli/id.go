package cli

import (
	"errors"
	"fmt"

	ucli "github.com/urfave/cli/v2"

	"github.com/fables-for-robots/kosh/state"
)

// idCommand is kosh id: it loads a state directory, creating what is
// missing, and prints what an operator hands to users.
func idCommand() *ucli.Command {
	return &ucli.Command{
		Name:  "id",
		Usage: "print the endpoint ID and the host key of a state directory, creating what is missing",
		Flags: []ucli.Flag{
			&ucli.StringFlag{
				Name:    "state-dir",
				EnvVars: []string{"KOSH_STATE_DIR"},
				Usage:   "the directory of the iroh key and the SSH host key",
			},
		},
		Action: func(c *ucli.Context) error {
			dir := c.String("state-dir")
			if dir == "" {
				return errors.New("--state-dir is required")
			}
			st, err := state.Load(dir)
			if err != nil {
				return fmt.Errorf("the state directory %s could not be loaded: %w", dir, err)
			}
			id := st.EndpointID()
			_, err = fmt.Fprintf(c.App.Writer,
				"endpoint ID   %s\n"+
					"iroh-ssh      iroh-ssh kosh@%s\n"+
					"host key      %s\n"+
					"fingerprint   %s\n",
				id, id, st.HostKeyLine(), st.HostKeyFingerprint())
			return err
		},
	}
}
