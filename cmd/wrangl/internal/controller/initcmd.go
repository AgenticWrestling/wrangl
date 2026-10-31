// Package controller implements controller mode: init, secrets and the serve process.
package controller

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nreid/devhost/cmd/wrangl/internal/config"
	"github.com/nreid/devhost/cmd/wrangl/internal/db"
	"github.com/nreid/devhost/cmd/wrangl/internal/db/dbgen"
	"github.com/nreid/devhost/cmd/wrangl/internal/stack"
)

// MetaSqidsAlphabet is the meta key holding the per-install sqids alphabet.
const MetaSqidsAlphabet = "sqids_alphabet"

// DBPath returns the database file for a data dir.
func DBPath(dataDir string) string { return filepath.Join(dataDir, "wrangl.db") }

// InitResult reports what Init created, for the CLI to print.
type InitResult struct {
	WroteConfig bool
	ConfigPath  string
	DBPath      string
}

// Init prepares a controller host. It is idempotent: existing config, database and
// alphabet are left untouched.
func Init(ctx context.Context, dataDir, configPath string) (*InitResult, error) {
	if dataDir == "" {
		return nil, errors.New("data dir must not be empty")
	}
	for _, d := range []struct {
		path string
		mode fs.FileMode
	}{
		{dataDir, 0o750},
		{filepath.Join(dataDir, "bin"), 0o750},
		{filepath.Join(dataDir, "nats"), 0o750},
		{SecretsDir(dataDir), 0o700},
	} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return nil, err
		}
		if err := os.Chmod(d.path, d.mode); err != nil { // MkdirAll is subject to umask
			return nil, err
		}
	}
	res := &InitResult{ConfigPath: configPath, DBPath: DBPath(dataDir)}

	if _, err := os.Stat(configPath); errors.Is(err, fs.ErrNotExist) {
		c := config.DefaultController()
		c.DataDir = dataDir
		b, err := c.Marshal()
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			return nil, err
		}
		header := "# wrangl controller config — see docs/superpowers/specs/2026-10-31-wrangl-design.md §12\n"
		if err := os.WriteFile(configPath, append([]byte(header), b...), 0o640); err != nil {
			return nil, err
		}
		res.WroteConfig = true
	} else if err != nil {
		return nil, err
	}

	d, err := db.Open(ctx, res.DBPath)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	if _, err := d.RQ.GetMeta(ctx, MetaSqidsAlphabet); errors.Is(err, sql.ErrNoRows) {
		alpha, err := stack.ShuffledAlphabet(nil)
		if err != nil {
			return nil, err
		}
		if err := d.Q.SetMetaIfAbsent(ctx, dbgen.SetMetaIfAbsentParams{Key: MetaSqidsAlphabet, Value: alpha}); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("read %s: %w", MetaSqidsAlphabet, err)
	}

	// M2: generate NATS operator/account keys and the TLS cert for the runner listener.
	// M8: generate VAPID keys for web push.
	return res, nil
}

// Backup writes a consistent copy of the live database to dest (SQLite VACUUM INTO).
// dest must not already exist.
func Backup(ctx context.Context, dataDir, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	d, err := db.Open(ctx, DBPath(dataDir))
	if err != nil {
		return err
	}
	defer d.Close()
	_, err = d.W.ExecContext(ctx, "VACUUM INTO ?", dest)
	return err
}
