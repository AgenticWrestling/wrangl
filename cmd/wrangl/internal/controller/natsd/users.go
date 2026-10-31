package natsd

import (
	"fmt"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nkeys"
)

// Subjects (spec §7). r = runner id.
func RunnerCmdSubject(r int64, cmd string) string { return fmt.Sprintf("wr.r.%d.cmd.%s", r, cmd) }
func RunnerHeartbeatSubject(r int64) string       { return fmt.Sprintf("wr.r.%d.hb", r) }
func RunnerInboxPrefix(r int64) string            { return fmt.Sprintf("_INBOX_%d", r) }

// RunnerTemplateEventSubject carries template apply results (JetStream WR_EVT).
func RunnerTemplateEventSubject(r int64) string { return fmt.Sprintf("wr.evt.%d.template", r) }

// TemplateEventWildcard matches every runner's template results.
const TemplateEventWildcard = "wr.evt.*.template"

// HeartbeatWildcard matches every runner's heartbeat.
const HeartbeatWildcard = "wr.r.*.hb"

// RunnerPermissions are the only subjects runner r may use. The controller derives the
// runner (and so its scope) from the subject a message arrived on, never the payload.
func RunnerPermissions(r int64) jwt.Permissions {
	var p jwt.Permissions
	p.Sub.Allow.Add(
		fmt.Sprintf("wr.r.%d.cmd.>", r),
		RunnerInboxPrefix(r)+".>",
	)
	p.Pub.Allow.Add(
		fmt.Sprintf("wr.evt.%d.>", r),
		fmt.Sprintf("wr.pty.%d.>", r),
		fmt.Sprintf("wr.stack.%d.>", r),
		RunnerHeartbeatSubject(r),
	)
	// Reply to requests the controller sends on wr.r.<r>.cmd.* (one reply each).
	p.Resp = &jwt.ResponsePermission{MaxMsgs: 1, Expires: time.Minute}
	return p
}

// IssueRunner signs a user JWT for runner r bound to the runner-generated public key.
// The runner keeps the private seed; the controller never sees it.
func (k *Keys) IssueRunner(r int64, userPub string) (string, error) {
	if !nkeys.IsValidPublicUserKey(userPub) {
		return "", fmt.Errorf("not a NATS user public key: %q", userPub)
	}
	uc := jwt.NewUserClaims(userPub)
	uc.Name = fmt.Sprintf("runner-%d", r)
	uc.IssuerAccount = pub(k.Account)
	uc.Permissions = RunnerPermissions(r)
	return uc.Encode(k.AccountSigning)
}

// controllerUser mints an unrestricted user for the controller's own connection,
// generated fresh on every start and never written to disk.
func (k *Keys) controllerUser() (userJWT string, kp nkeys.KeyPair, err error) {
	if kp, err = nkeys.CreateUser(); err != nil {
		return "", nil, err
	}
	uc := jwt.NewUserClaims(pub(kp))
	uc.Name = "controller"
	uc.IssuerAccount = pub(k.Account)
	userJWT, err = uc.Encode(k.AccountSigning)
	return userJWT, kp, err
}
