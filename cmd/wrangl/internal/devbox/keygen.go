// Package devbox holds what `wrangl dev` needs besides AWS calls: the SSH key and ssh_config
// block, the default dev toolchain profile, and the idle-stop unit.
package devbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	gossh "golang.org/x/crypto/ssh"
)

// KeyName is the dedicated key `wrangl dev` uses, so the user's own id_ed25519 is never
// created, replaced or pushed to instances.
const KeyName = "wrangl_dev_ed25519"

// KeyPath returns ~/.ssh/wrangl_dev_ed25519.
func KeyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ssh", KeyName), nil
}

// EnsureKeyPair returns the public key (authorized_keys format), generating the key pair if absent.
func EnsureKeyPair() ([]byte, error) {
	priv, err := KeyPath()
	if err != nil {
		return nil, err
	}
	pubPath := priv + ".pub"
	if _, err := os.Stat(priv); err == nil {
		pub, err := os.ReadFile(pubPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%s exists but %s is missing: restore the public key or delete the private key to regenerate", priv, pubPath)
			}
			return nil, err
		}
		return pub, nil
	}
	if err := os.MkdirAll(filepath.Dir(priv), 0o700); err != nil {
		return nil, err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := gossh.MarshalPrivateKey(key, "wrangl dev")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(priv, pem.EncodeToMemory(blk), 0o600); err != nil {
		return nil, err
	}
	sp, err := gossh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	b := gossh.MarshalAuthorizedKey(sp)
	return b, os.WriteFile(pubPath, b, 0o644)
}
