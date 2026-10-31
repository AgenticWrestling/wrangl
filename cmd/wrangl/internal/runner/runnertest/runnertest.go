// Package runnertest provides test doubles for runner-side tools. Nothing here touches
// the network.
package runnertest

import (
	"context"
	"io"
	"os"
	"path/filepath"
)

// FakeMise is a stand-in for the mise binary. Each call appends
// "<args> HOME=<home> USER=<user>" to $HOME/mise-calls.log; `install` fails if the
// global config contains the word FAIL.
const FakeMise = `#!/bin/sh
echo "$* HOME=$HOME USER=$USER" >> "$HOME/mise-calls.log"
if [ "$1" = "install" ] && grep -q FAIL "$HOME/.config/mise/config.toml"; then
  echo "mise ERROR tool FAIL@latest not found" >&2
  exit 1
fi
echo "mise: all tools installed"
`

// MiseInstaller "installs" FakeMise at dest. calls, if non-nil, counts installs.
func MiseInstaller(calls *int) func(context.Context, string, io.Writer) error {
	return func(_ context.Context, dest string, out io.Writer) error {
		if calls != nil {
			*calls++
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		io.WriteString(out, "fake mise installer\n")
		return os.WriteFile(dest, []byte(FakeMise), 0o755)
	}
}
