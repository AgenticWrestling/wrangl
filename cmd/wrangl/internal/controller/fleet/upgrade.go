package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/natsd"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/db/dbgen"
	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/proto"
)

// ResendUpgradeAfter is how long the controller waits for an upgraded heartbeat before
// sending cmd.upgrade again.
var ResendUpgradeAfter = 2 * time.Minute

// reconcileBinary keeps runners on the binary this controller serves. The sha256 is the
// version (dev builds all say "dev"). A runner on another binary is marked (no new runs
// are claimed by it); once it is idle it is told to upgrade, and the mark clears when it
// heartbeats on the new binary. Runners that report no hash have self-upgrade off.
func (s *Service) reconcileBinary(ctx context.Context, id int64, hb proto.Heartbeat) error {
	if hb.BinarySHA == "" {
		return nil
	}
	r, err := s.DB.RQ.GetRunner(ctx, id)
	if err != nil {
		return err
	}
	if err := s.DB.Q.SetRunnerBinary(ctx, dbgen.SetRunnerBinaryParams{BinarySha: hb.BinarySHA, ID: id}); err != nil {
		return err
	}
	sums, err := s.binarySums()
	if err != nil {
		return err
	}
	want := sums[unameArch(r.Arch)] // r.Arch is the EC2 name (arm64); sums are keyed by uname -m
	if want == "" {
		return nil // nothing to serve for this arch
	}
	if hb.BinarySHA == want {
		if r.UpgradeSha != "" {
			s.Log.Info("runner upgraded", "runner", r.Name, "sha256", short(want))
			s.note(ctx, "upgrade", "info", r.Name, "Runner binary upgraded", short(want))
			s.Bus.Runner(r.Name)
			return s.DB.Q.ClearUpgrade(ctx, id)
		}
		return nil
	}
	if r.UpgradeSha != want {
		s.Log.Info("runner binary out of date; upgrade pending", "runner", r.Name, "has", short(hb.BinarySHA), "want", short(want))
		if err := s.DB.Q.RequestUpgrade(ctx, dbgen.RequestUpgradeParams{UpgradeSha: want, ID: id}); err != nil {
			return err
		}
		s.Bus.Runner(r.Name)
	}
	if hb.RunID != 0 || hb.Applying || hb.Upgrading || s.NC == nil {
		return nil // after the run / apply; or it is already on it
	}
	now := s.now()
	n, err := s.DB.Q.MarkUpgradeSent(ctx, dbgen.MarkUpgradeSentParams{Now: nullStr(ts(now)), ID: id, ResendBefore: nullStr(ts(now.Add(-ResendUpgradeAfter)))})
	if err != nil || n == 0 {
		return err // not idle, or sent recently
	}
	b, _ := json.Marshal(proto.Upgrade{SHA256: want})
	msg, err := s.NC.RequestWithContext(ctx, natsd.RunnerCmdSubject(id, "upgrade"), b)
	if err != nil {
		return fmt.Errorf("send upgrade to %s: %w", r.Name, err)
	}
	var ack proto.Ack
	if json.Unmarshal(msg.Data, &ack) != nil || !ack.Accepted {
		return fmt.Errorf("%s declined the upgrade: %s", r.Name, ack.Reason)
	}
	s.Log.Info("upgrade sent", "runner", r.Name, "sha256", short(want))
	s.note(ctx, "upgrade", "info", r.Name, "Runner binary upgrade sent", short(hb.BinarySHA)+" → "+short(want))
	return nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
