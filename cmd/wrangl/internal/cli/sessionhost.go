package cli

import (
	"context"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/sessionhost"
)

// DefaultSessionSocket is where the session host listens unless told otherwise.
func DefaultSessionSocket() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "wrangl", "sessions.sock")
}

func sessionHostCmd() *cobra.Command {
	var sock, group string
	c := &cobra.Command{
		Use:   "session-host",
		Short: "Run the session host: owns the PTYs of primary Claude sessions (systemd user unit wrangl-sessions)",
		Long: "Runs until stopped. Primary sessions live in this process, apart from the controller, so a controller\n" +
			"restart does not end them. Stopping or upgrading this service ends them; they resume with `claude -c`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()
			return runSessionHost(ctx, sock, group)
		},
	}
	c.Flags().StringVar(&sock, "socket", DefaultSessionSocket(), "unix socket to serve (mode 0660)")
	c.Flags().StringVar(&group, "group", sessionhost.DefaultGroup, "group that owns the socket")
	return c
}

func runSessionHost(ctx context.Context, sock, group string) error {
	h := sessionhost.NewHost(sessionhost.Config{})
	defer h.StopAll()
	return (&sessionhost.Server{Host: h, Path: sock, Group: group}).Serve(ctx)
}
