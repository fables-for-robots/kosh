// Package state is what kosh keeps between starts: its iroh key, whose
// public half is the endpoint ID, and its SSH host key. Both are files
// in one directory.
package state

import (
	irohkey "github.com/tmc/go-iroh/key"
	"golang.org/x/crypto/ssh"
)

// The files of a state directory.
const (
	// IrohKeyFile holds the iroh key: 64 hex characters and a line feed.
	IrohKeyFile = "iroh.key"
	// HostKeyFile holds the SSH host key: an ed25519 key in OpenSSH's
	// private key format.
	HostKeyFile = "ssh_host_ed25519_key"
)

// State is the two keys of a state directory.
type State struct {
	IrohKey irohkey.SecretKey
	// HostKey signs for the SSH server.
	HostKey ssh.Signer
	// HostKeyPEM is the host key as its file holds it.
	HostKeyPEM []byte
}

// Load reads the two keys in dir. A file that is missing is created with
// a new key, mode 0600, and dir with mode 0700 when it is missing too. A
// file that is there and does not parse is an error that names the path
// and repeats none of the contents, and the file is left as it is.
func Load(dir string) (State, error) {
	panic("not implemented")
}

// EndpointID returns the endpoint ID: 64 hex characters.
func (s State) EndpointID() string {
	panic("not implemented")
}

// HostKeyLine returns the host key's public half as a known_hosts or
// authorized_keys line has it: "ssh-ed25519 AAAA…", without a comment
// and without a line feed.
func (s State) HostKeyLine() string {
	panic("not implemented")
}

// HostKeyFingerprint returns "SHA256:…" of the host key.
func (s State) HostKeyFingerprint() string {
	panic("not implemented")
}
