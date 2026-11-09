package menu

import (
	"context"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
)

// Chooser is provision.Provisioner's legacy instance choice.
type Chooser interface {
	ChooseInstanceType(context.Context, provision.Spec) (string, error)
}

// Legacy is the menu of a template that declares none: the single type provisioning picks today (the
// smallest current-generation type meeting the template's minimums), so old templates keep launching.
func Legacy(ctx context.Context, c Chooser, s provision.Spec) ([]Entry, error) {
	t, err := c.ChooseInstanceType(ctx, s)
	if err != nil {
		return nil, err
	}
	return []Entry{{Type: t}}, nil
}
