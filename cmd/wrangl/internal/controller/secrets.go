package controller

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

// SecretsDir holds agent and integration secrets, one file per secret, mode 0600.
func SecretsDir(dataDir string) string { return filepath.Join(dataDir, "secrets") }

var secretName = regexp.MustCompile(`^[a-z0-9_.-]+$`)

// ErrBadSecretName rejects names that could escape the secrets dir or be ambiguous.
var ErrBadSecretName = errors.New("secret names must match [a-z0-9_.-]+")

func secretPath(dataDir, name string) (string, error) {
	if !secretName.MatchString(name) || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q", ErrBadSecretName, name)
	}
	return filepath.Join(SecretsDir(dataDir), name), nil
}

// SecretSet atomically writes the value read from r (trailing newline trimmed).
func SecretSet(dataDir, name string, r io.Reader) error {
	p, err := secretPath(dataDir, name)
	if err != nil {
		return err
	}
	v, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return err
	}
	for len(v) > 0 && (v[len(v)-1] == '\n' || v[len(v)-1] == '\r') {
		v = v[:len(v)-1]
	}
	if len(v) == 0 {
		return errors.New("empty secret value on stdin")
	}
	if err := os.MkdirAll(SecretsDir(dataDir), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(SecretsDir(dataDir), ".tmp-"+name+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(v); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// SecretGet reads a secret's value.
func SecretGet(dataDir, name string) ([]byte, error) {
	p, err := secretPath(dataDir, name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// SecretList returns secret names only, never values.
func SecretList(dataDir string) ([]string, error) {
	ents, err := os.ReadDir(SecretsDir(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if e.Type().IsRegular() && secretName.MatchString(e.Name()) && e.Name()[0] != '.' {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// SecretRm deletes a secret.
func SecretRm(dataDir, name string) error {
	p, err := secretPath(dataDir, name)
	if err != nil {
		return err
	}
	return os.Remove(p)
}
