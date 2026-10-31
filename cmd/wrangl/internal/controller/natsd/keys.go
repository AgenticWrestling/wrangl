// Package natsd configures the controller's embedded NATS server in operator/JWT mode
// and issues per-runner user credentials (spec §7).
//
// Layout under <data-dir>/nats/: operator.nk, sys.nk, account.nk, account-signing.nk
// (all 0600 nkey seeds) and ca.pem / ca-key.pem for the runner-facing TLS listener.
package natsd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// AccountName is the account every runner and the controller live in.
const AccountName = "WRANGL"

// Keys holds the operator hierarchy loaded from disk.
type Keys struct {
	Operator       nkeys.KeyPair
	System         nkeys.KeyPair // system account
	Account        nkeys.KeyPair // WRANGL account identity
	AccountSigning nkeys.KeyPair // signs user JWTs; rotatable without touching the account
}

var seedFiles = []struct {
	name   string
	create func() (nkeys.KeyPair, error)
}{
	{"operator.nk", nkeys.CreateOperator},
	{"sys.nk", nkeys.CreateAccount},
	{"account.nk", nkeys.CreateAccount},
	{"account-signing.nk", nkeys.CreateAccount},
}

// EnsureKeys creates any missing seed files in dir and loads all of them. Existing seeds
// are never replaced: rotating them would invalidate every issued runner credential.
func EnsureKeys(dir string) (*Keys, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	kps := make([]nkeys.KeyPair, len(seedFiles))
	for i, sf := range seedFiles {
		p := filepath.Join(dir, sf.name)
		seed, err := os.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			kp, err := sf.create()
			if err != nil {
				return nil, err
			}
			if seed, err = kp.Seed(); err != nil {
				return nil, err
			}
			if err := os.WriteFile(p, seed, 0o600); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		}
		if kps[i], err = nkeys.FromSeed(seed); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return &Keys{Operator: kps[0], System: kps[1], Account: kps[2], AccountSigning: kps[3]}, nil
}

func pub(kp nkeys.KeyPair) string {
	p, err := kp.PublicKey()
	if err != nil {
		panic(err) // only fails for a corrupt in-memory key
	}
	return p
}

// claims builds the operator, system-account and WRANGL-account JWTs.
func (k *Keys) claims() (op *jwt.OperatorClaims, sysJWT, accJWT string, err error) {
	op = jwt.NewOperatorClaims(pub(k.Operator))
	op.Name = "wrangl"
	op.SystemAccount = pub(k.System)
	opJWT, err := op.Encode(k.Operator)
	if err != nil {
		return nil, "", "", err
	}
	if op, err = jwt.DecodeOperatorClaims(opJWT); err != nil {
		return nil, "", "", err
	}

	sys := jwt.NewAccountClaims(pub(k.System))
	sys.Name = "SYS"
	if sysJWT, err = sys.Encode(k.Operator); err != nil {
		return nil, "", "", err
	}

	acc := jwt.NewAccountClaims(pub(k.Account))
	acc.Name = AccountName
	acc.SigningKeys.Add(pub(k.AccountSigning))
	acc.Limits.JetStreamLimits = jwt.JetStreamLimits{MemoryStorage: -1, DiskStorage: -1, Streams: -1, Consumer: -1}
	if accJWT, err = acc.Encode(k.Operator); err != nil {
		return nil, "", "", err
	}
	return op, sysJWT, accJWT, nil
}
