package fleet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/agenticwrestling/wrangl/cmd/wrangl/internal/controller/provision"
)

var probes sync.Map // token -> chan *provision.ProbeReport

// SubmitProbe delivers a test instance's report. It reports false for an unknown token.
func (s *Service) SubmitProbe(token string, r *provision.ProbeReport) bool {
	v, ok := probes.Load(token)
	if !ok {
		return false
	}
	select {
	case v.(chan *provision.ProbeReport) <- r:
	default: // already reported
	}
	return true
}

// TestImage launches one instance of ami on itype, has it report on what the runner bootstrap
// needs (cloud-init user-data, systemd, curl, outbound HTTPS, the enrol port), and terminates
// it. About two minutes and a few cents.
func (s *Service) TestImage(ctx context.Context, ami, itype, arch string) (provision.ImageTest, error) {
	if s.Prov == nil {
		return provision.ImageTest{}, ErrNoAWS
	}
	if s.EnrollURL == "" {
		return provision.ImageTest{}, fmt.Errorf("%w: the controller has no enrol URL", ErrInvalid)
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return provision.ImageTest{}, err
	}
	token := hex.EncodeToString(b[:])
	ud, err := provision.ProbeScript(s.EnrollURL, token)
	if err != nil {
		return provision.ImageTest{}, err
	}
	ch := make(chan *provision.ProbeReport, 1)
	probes.Store(token, ch)
	defer probes.Delete(token)
	res := s.Prov.TestImage(ctx, provision.Spec{
		Runner: "imagetest-" + token[:8], Template: "imagetest", Arch: arch, DiskGiB: 20,
		InstanceTypes: []string{itype}, AMI: ami,
		SubnetID: s.SubnetID, SecurityGroupID: s.SecurityGroupID, UserData: ud,
	}, func(c context.Context) (*provision.ProbeReport, error) {
		select {
		case r := <-ch:
			return r, nil
		case <-c.Done():
			return nil, c.Err()
		}
	})
	s.note(ctx, "image", map[bool]string{true: "info", false: "warn"}[res.OK], ami, "Image test "+map[bool]string{true: "passed", false: "failed"}[res.OK],
		fmt.Sprintf("instance %s on %s; terminated=%v %s", res.InstanceID, itype, res.Terminated, res.Error))
	return res, nil
}
