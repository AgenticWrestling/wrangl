package cli

import (
	"github.com/spf13/cobra"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db"
)

func dbMigrate(cmd *cobra.Command, path, direction string) error {
	return db.Migrate(cmd.Context(), path, direction, cmd.OutOrStdout())
}
