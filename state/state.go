// Package state is what kosh keeps between starts: its iroh key, whose
// public half is the endpoint ID, and its SSH host key. Both are files
// in one directory.
package state

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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

const (
	// seedHexLen is the length of the iroh seed written in hex.
	seedHexLen = 2 * irohkey.SeedSize
	// maxIrohKeyFile bounds what is read of iroh.key. A seed with white
	// space around it fits many times over.
	maxIrohKeyFile = 1024
	// maxHostKeyFile bounds what is read of the host key file. An RSA key
	// of 16384 bits is well below it.
	maxHostKeyFile = 1 << 20
	// keyFileSpace is the white space allowed around the iroh seed.
	keyFileSpace = " \t\r\n"
)

// Load reads the two keys in dir. A file that is missing is created with
// a new key, mode 0600, and dir with mode 0700 when it is missing too. A
// file that is there and does not parse is an error that names the path
// and repeats none of the contents, and the file is left as it is.
//
// A file is never written over. A new one is written under a temporary
// name and hard-linked to its name, so it has that name only once it
// holds the whole key; this takes a file system with hard links. Several
// Loads of one empty directory, in one process or several, all return the
// keys of whichever created the files first. A directory that cannot be
// written to is fine when both files are there.
func Load(dir string) (State, error) {
	var s State
	irohPath := filepath.Join(dir, IrohKeyFile)
	content, err := loadOrCreate(dir, irohPath, maxIrohKeyFile, newIrohKeyFile)
	if err != nil {
		return State{}, err
	}
	if s.IrohKey, err = parseIrohKey(content); err != nil {
		return State{}, fmt.Errorf("key file %s: %w", irohPath, err)
	}

	hostPath := filepath.Join(dir, HostKeyFile)
	if s.HostKeyPEM, err = loadOrCreate(dir, hostPath, maxHostKeyFile, newHostKeyFile); err != nil {
		return State{}, err
	}
	if s.HostKey, err = parseHostKey(s.HostKeyPEM); err != nil {
		return State{}, fmt.Errorf("key file %s: %w", hostPath, err)
	}
	return s, nil
}

// loadOrCreate returns the contents of the file at path, at most limit
// bytes and one more when it is longer. When the file is missing it makes
// the contents with gen and creates the file first; when another Load
// created it in the meantime, it returns what that one wrote.
func loadOrCreate(dir, path string, limit int64, gen func() ([]byte, error)) ([]byte, error) {
	content, err := readLimited(path, limit)
	if !errors.Is(err, fs.ErrNotExist) {
		return content, err
	}
	fresh, err := gen()
	if err != nil {
		return nil, fmt.Errorf("create key file %s: %w", path, err)
	}
	if err := createFile(dir, path, fresh); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create key file %s: %w", path, err)
		}
		return readLimited(path, limit)
	}
	return fresh, nil
}

// readLimited reads at most limit+1 bytes of the file at path. Its errors
// name the path; a missing file wraps fs.ErrNotExist.
func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read key file: %w", err)
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read key file %s: %w", path, err)
	}
	return content, nil
}

// createFile writes content to a new file at path with mode 0600, making
// dir first when it is missing. It fails with an error that wraps
// fs.ErrExist when there is a file at path already.
func createFile(dir, path string, content []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(content)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Link(tmp.Name(), path)
}

func newIrohKeyFile() ([]byte, error) {
	sk, err := irohkey.GenerateSecretKey()
	if err != nil {
		return nil, err
	}
	seed := sk.Bytes()
	return []byte(hex.EncodeToString(seed[:]) + "\n"), nil
}

func newHostKeyFile() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(blk), nil
}

// parseIrohKey parses the contents of iroh.key. Its errors say what is
// wrong and repeat none of the contents, which may be most of a key.
func parseIrohKey(content []byte) (irohkey.SecretKey, error) {
	if len(content) > maxIrohKeyFile {
		return irohkey.SecretKey{}, fmt.Errorf("more than %d bytes, want %d hex characters", maxIrohKeyFile, seedHexLen)
	}
	text := bytes.Trim(content, keyFileSpace)
	if len(text) != seedHexLen {
		return irohkey.SecretKey{}, fmt.Errorf("%d characters, want %d of hex", len(text), seedHexLen)
	}
	var seed [irohkey.SeedSize]byte
	if _, err := hex.Decode(seed[:], text); err != nil {
		return irohkey.SecretKey{}, errors.New("not hex")
	}
	if seed == [irohkey.SeedSize]byte{} {
		return irohkey.SecretKey{}, errors.New("the seed is all zeros, which is no key")
	}
	return irohkey.NewSecretKey(seed), nil
}

// parseHostKey parses the contents of the host key file. Its errors repeat
// none of the contents.
func parseHostKey(content []byte) (ssh.Signer, error) {
	if len(content) > maxHostKeyFile {
		return nil, fmt.Errorf("more than %d bytes", maxHostKeyFile)
	}
	key, err := ssh.ParseRawPrivateKey(content)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, errors.New("the key has a passphrase, which kosh cannot ask for")
		}
		return nil, errors.New("not a private key in a format ssh knows")
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, errors.New("a private key of a type that cannot sign")
	}
	return signer, nil
}

// EndpointID returns the endpoint ID: 64 hex characters.
func (s State) EndpointID() string {
	pub := s.IrohKey.Public().Bytes()
	return strings.ToLower(hex.EncodeToString(pub[:]))
}

// HostKeyLine returns the host key's public half as a known_hosts or
// authorized_keys line has it: "ssh-ed25519 AAAA…", without a comment
// and without a line feed.
func (s State) HostKeyLine() string {
	return strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(s.HostKey.PublicKey())), "\n")
}

// HostKeyFingerprint returns "SHA256:…" of the host key.
func (s State) HostKeyFingerprint() string {
	return ssh.FingerprintSHA256(s.HostKey.PublicKey())
}
