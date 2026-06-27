// cmd/ssm/internal/ssh/keygen.go
package ssh

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	gossh "golang.org/x/crypto/ssh"
)

func EnsureKeyPair() ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	sshDir := filepath.Join(home, ".ssh")
	privPath := filepath.Join(sshDir, "id_ed25519")
	pubPath := privPath + ".pub"

	if _, err := os.Stat(privPath); err == nil {
		pub, err := os.ReadFile(pubPath)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("~/.ssh/id_ed25519 exists but id_ed25519.pub is missing — restore the public key or delete id_ed25519 to regenerate")
			}
			return nil, err
		}
		return pub, nil
	}

	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, err
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}

	privPEM, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(privPath, pem.EncodeToMemory(privPEM), 0600); err != nil {
		return nil, err
	}

	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		return nil, err
	}
	pubBytes := gossh.MarshalAuthorizedKey(sshPub)
	if err := os.WriteFile(pubPath, pubBytes, 0644); err != nil {
		return nil, err
	}
	return pubBytes, nil
}
