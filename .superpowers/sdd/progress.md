# SSM Tool — SDD Progress Ledger

## Plan Files
- M1: docs/superpowers/plans/2026-06-26-ssm-m1-scaffold.md
- M2: docs/superpowers/plans/2026-06-26-ssm-m2-connect.md
- M3: docs/superpowers/plans/2026-06-26-ssm-m3-tunnel.md
- M4: docs/superpowers/plans/2026-06-26-ssm-m4-setup-eic.md
- M5: docs/superpowers/plans/2026-06-26-ssm-m5-setup-provision.md

## Tasks

### M1
- [ ] M1-T1: Go module + directory scaffold
- [ ] M1-T2: Profile listing (ListProfiles)
- [ ] M1-T3: Instance resolver (ResolveInstance, ListInstances, ParseRegionName)
- [ ] M1-T4: Interactive picker + spinner
- [ ] M1-T5: resolveTarget shared helper

### M2
- [ ] M2-T1: ssm connect subcommand

### M3
- [ ] M3-T1: Port spec parser
- [ ] M3-T2: ssm tunnel subcommand

### M4
- [ ] M4-T1: SSH keypair + config helpers
- [ ] M4-T2: EIC endpoint management + key push
- [ ] M4-T3: VPC security group helper
- [ ] M4-T4: Wire Phase 1 into ssm setup

### M5
- [ ] M5-T1: CloudWatch Logs tail helper
- [ ] M5-T2: Remote provisioning script sender
- [ ] M5-T3: Wire Phase 2+3 into ssm setup
- [ ] M5-T4: Final build and install
Task 1: complete (commits 885c471..d55439b, review clean)
Task 2: complete (commits 02943cc..99f4102, review clean)
Task 3: complete (commits 99f4102..6fe39b7, review clean)
Task 4: complete (commits 6fe39b7..fdf2a51, review clean)
Task 5: complete (commits fdf2a51..1cdcddc, review clean)
Task M2-T1: complete (commits 1cdcddc..e73062b, review clean)
Task M3-T1+T2: complete (commits e73062b..8fa00b4, review clean. Minor: waitForPort dial timeout 1s reduces effective polls to ~10 in 15s window)
Task M4-T1: complete (commits 8fa00b4..7f450a5, review clean + MkdirAll fix + CutPrefix style)
Task M4-T2: complete (commits 7f450a5..5af8895, review clean)
Task M4-T3: complete (commits 5af8895..374322d, review clean)
Task M4-T4: complete (commits 374322d..f80d9dc, review clean after spinner fix)
Task M5-T1: complete (commits f80d9dc..6d82525, review clean)
Task M5-T2: complete (commits 6d82525..b14903e, review clean)
Task M5-T3: complete (commits b14903e..1824985, review clean)
