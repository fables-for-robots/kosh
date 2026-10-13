//go:build !unix || solaris

package irohnet

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"

	irohkey "github.com/tmc/go-iroh/key"
)

// listenMDNS fails: the mDNS port is shared with SO_REUSEPORT, which this
// platform (Windows, Solaris and illumos among them) does not offer as
// golang.org/x/sys/unix does elsewhere, and kosh is not announced on the
// local network here. Listen warns of it and goes on without mDNS.
func listenMDNS(_ context.Context, _ irohkey.EndpointID, _ *slog.Logger) (*mdnsAnnouncer, error) {
	return nil, fmt.Errorf("mdns: not available on %s", runtime.GOOS)
}
