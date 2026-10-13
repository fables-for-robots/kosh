package sshd

import (
	gossh "golang.org/x/crypto/ssh"
)

// What a session channel may ask, and how much.
const (
	// maxEnvRequests is how many env requests of a channel are passed on.
	maxEnvRequests = 64
	// maxEnvBytes is how many bytes of names and values, in all, the env
	// requests of a channel may pass on.
	maxEnvBytes = 16 << 10
	// maxRequestPayload is the largest payload of any request that is
	// passed on.
	maxRequestPayload = 64 << 10
)

// passedRequests are the requests of a session channel that the library
// is given: kosh has a use for them. Any other, an agent's, a
// subsystem's, X11's, a signal or one kosh does not know, is refused
// before the library sees it.
var passedRequests = map[string]bool{
	"pty-req":       true,
	"shell":         true,
	"exec":          true,
	"window-change": true,
	"env":           true,
}

// filteredChannel is a new session channel whose requests are filtered
// once it is accepted.
type filteredChannel struct {
	gossh.NewChannel
	// a is told the channel when it is accepted: see rawWriter.
	a *accepted
}

// Accept accepts the channel, and returns, in place of its requests,
// those that filterRequests passes on.
func (f filteredChannel) Accept() (gossh.Channel, <-chan *gossh.Request, error) {
	ch, in, err := f.NewChannel.Accept()
	if err != nil {
		return ch, in, err
	}
	f.a.set(ch)
	out := make(chan *gossh.Request)
	go filterRequests(in, out)
	return ch, out, nil
}

// filterRequests passes the requests of in that a session channel may
// make on to out, and refuses the others, with a reply when one is
// wanted: a type that is not among passedRequests, a payload larger than
// maxRequestPayload, an env request beyond maxEnvRequests or
// maxEnvBytes, or one that does not parse. The library appends every
// env request it is given to a slice of the channel's. out is closed
// when in is, which x/crypto does when the channel is closed.
func filterRequests(in <-chan *gossh.Request, out chan<- *gossh.Request) {
	defer close(out)
	envs, envBytes := 0, 0
	for req := range in {
		pass := passedRequests[req.Type] && len(req.Payload) <= maxRequestPayload
		if pass && req.Type == "env" {
			var kv struct{ Name, Value string }
			if err := gossh.Unmarshal(req.Payload, &kv); err != nil {
				pass = false
			} else if n := len(kv.Name) + len(kv.Value); envs >= maxEnvRequests || envBytes+n > maxEnvBytes {
				pass = false
			} else {
				envs++
				envBytes += n
			}
		}
		if !pass {
			// Reply does nothing when no reply is wanted.
			_ = req.Reply(false, nil)
			continue
		}
		out <- req
	}
}
