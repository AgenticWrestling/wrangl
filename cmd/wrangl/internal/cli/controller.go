package cli

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/config"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller"
)

// controllerConfig resolves the controller config path and loads it, letting
// --data-dir override data_dir.
func controllerConfig(g *Global, dataDir string) (*config.Controller, error) {
	path := config.Resolve(g.Config, "WRANGL_CONFIG", "", config.DefaultControllerConfig)
	c, err := config.LoadController(path)
	if err != nil {
		return nil, err
	}
	if dataDir != "" {
		c.DataDir = dataDir
	}
	return c, nil
}

func controllerCmd(g *Global) *cobra.Command {
	var dataDir string
	dataDirFlag := func(c *cobra.Command) *cobra.Command {
		c.Flags().StringVar(&dataDir, "data-dir", "", "data directory (default: data_dir from config)")
		return c
	}

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Prepare this host: data dir, config skeleton, database, keys",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath := config.Resolve(g.Config, "WRANGL_CONFIG", "", config.DefaultControllerConfig)
			dir := dataDir
			if dir == "" {
				if c, err := config.LoadController(cfgPath); err == nil {
					dir = c.DataDir
				} else {
					dir = config.DefaultDataDir
				}
			}
			res, err := controller.Init(cmd.Context(), dir, cfgPath)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if res.WroteConfig {
				fmt.Fprintf(out, "wrote config   %s\n", res.ConfigPath)
			} else {
				fmt.Fprintf(out, "kept config    %s\n", res.ConfigPath)
			}
			fmt.Fprintf(out, "database ready %s\n", res.DBPath)
			return nil
		},
	}

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the controller (database, embedded NATS, scheduler, HTTP)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := controllerConfig(g, dataDir)
			if err != nil {
				return err
			}
			s, err := controller.New(c, slog.New(slog.NewTextHandler(os.Stderr, nil)))
			if err != nil {
				return err
			}
			return s.Run(cmd.Context())
		},
	}

	migrate := &cobra.Command{
		Use:       "migrate [up|down|status]",
		Short:     "Run database migrations (serve also migrates up on start)",
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"up", "down", "status"},
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := controllerConfig(g, dataDir)
			if err != nil {
				return err
			}
			dir := "up"
			if len(args) == 1 {
				dir = args[0]
			}
			return dbMigrate(cmd, controller.DBPath(c.DataDir), dir)
		},
	}

	backup := &cobra.Command{
		Use:   "backup <path>",
		Short: "Write a consistent copy of the database to path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := controllerConfig(g, dataDir)
			if err != nil {
				return err
			}
			return controller.Backup(cmd.Context(), c.DataDir, args[0])
		},
	}

	secretDir := func() (string, error) {
		c, err := controllerConfig(g, dataDir)
		if err != nil {
			return "", err
		}
		return c.DataDir, nil
	}
	secret := group("secret", "Manage controller secrets (agent tokens, keys)",
		&cobra.Command{
			Use:   "set <name>",
			Short: "Set a secret from stdin, e.g. `claude setup-token | wrangl controller secret set claude_oauth_token`",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				dir, err := secretDir()
				if err != nil {
					return err
				}
				return controller.SecretSet(dir, args[0], cmd.InOrStdin())
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List secret names (never values)",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				dir, err := secretDir()
				if err != nil {
					return err
				}
				names, err := controller.SecretList(dir)
				for _, n := range names {
					fmt.Fprintln(cmd.OutOrStdout(), n)
				}
				return err
			},
		},
		&cobra.Command{
			Use:   "rm <name>",
			Short: "Delete a secret",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				dir, err := secretDir()
				if err != nil {
					return err
				}
				return controller.SecretRm(dir, args[0])
			},
		},
	)
	for _, c := range []*cobra.Command{initCmd, serve, migrate, backup} {
		dataDirFlag(c)
	}
	secret.PersistentFlags().StringVar(&dataDir, "data-dir", "", "data directory (default: data_dir from config)")

	return group("controller", "Controller mode (runs on the controller host)", initCmd, serve, migrate, backup, secret)
}
