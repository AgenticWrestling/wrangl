# Android emulator runner (experimental)

How to let a wrangl agent build an Android app and exercise it on an emulator. It uses the remote
runner mechanism ([ADR 0002](adr/0002-remote-runners.md), `INSTALL.md` section 8.2): the **agent runs
on a driver**, an ordinary runner, with the repo checked out there. The **emulator host** is a
*target*: a machine that runs no wrangl binary, which the controller launches, probes and reaps. The
agent reaches the emulator's adb through two tools served by the driver's daemon.

```
 controller ── launches/probes/reaps ──────────────► target (emulator host, no wrangl)
     │                                                   ▲  emulator, adb on 127.0.0.1:5555
     └─ assigns the run ─► driver (local runner)         │
                            agent + checkout   ──adb──►  SSM port-forward (no inbound ports)
                            target_put / target_exec
```

Everything below was checked against the code. Nothing here has been run on a real emulator host:
the ssh/adb/SSM code paths are exercised in tests only with a fake target.

## 1. What runs where

| | Driver (local runner) | Target (emulator host) |
|---|---|---|
| wrangl binary | yes | **no** |
| agent, git checkout, Gradle/JDK/Android SDK build | yes | no |
| emulator, system image, `adbd` | no | yes |
| `aws` CLI + Session Manager plugin, `adb` client | yes (the daemon starts them) | no |
| SSM agent | no | yes |

An APK is **built on the driver** and then copied to the emulator. `target_exec` on an Android
target runs `adb shell`, i.e. a command on the *device*, so it cannot run Gradle. This differs from
the Mac template, where `target_exec` is a real shell on the build machine.

## 2. The instance: KVM, and what it costs

The emulator is only usable with hardware acceleration (KVM). On EC2 that means:

- a **bare-metal** instance type (for example `c5.metal`, x86_64), or
- a type that supports **nested virtualisation**. AWS has offered this on some newer Intel
  families; check the current EC2 documentation for which before relying on one.

Trade-off: metal is always KVM-capable but is the largest and dearest option and is billed for
the whole time the instance runs; nested virtualisation is smaller but slower than metal and depends
on AWS's current support. The example template lists `c5.metal` as the safe choice. List the types
explicitly: when `instance_types` is empty wrangl picks the cheapest type that satisfies `min`
(`ChooseInstanceType`), which does not look at KVM. wrangl does not show a price in this template;
use the EC2 pricing page for the type you pick.

Billing is per running instance, so the controls that matter are:

- **Project cap, default 0.** A project's cap for a template is 1 only if it is the project's default
  template, otherwise 0 (migration 00025 seeds this; later projects/templates get the same rule).
  Opt in with `wrangl project edit PROJ --limit android=1`; see it with `wrangl project show PROJ`.
  The effective cap is also bounded by the template's `max_runners` (1 in the example).
- **`idle_timeout`.** The controller reaps a target with no sessions after this long (default 30m,
  20m in the example). A booted emulator is reused by back-to-back runs while it lasts.
- **`wrangl target list`** shows live targets, their state and sessions; **`wrangl target terminate
  NAME`** stops one now (runs using it fail as "target lost").

The cost warning wrangl prints on launch is currently only for Mac instance types (`mac*`); there
is none for Android, so the cap is your safeguard.

## 3. Prepare the driver

The driver is any runner template that allows the agent and is named in `connection.driver_template`.
Its image needs, in the daemon's `PATH` (the real backend runs `aws`, `adb`, `scp` by name):

- the `aws` CLI and the **Session Manager plugin** (the tunnel is `aws ssm start-session --target
  INSTANCE --document-name AWS-StartPortForwardingSession --parameters
  portNumber=5555,localPortNumber=N`);
- `adb` (platform-tools);
- whatever builds your app: a JDK and the Android SDK (so `./gradlew assembleDebug` works there).

Add these in the driver template's `setup_script` / `mise_toml`. Runners are launched with the
instance profile named by `aws.runner_instance_profile` in the controller config; the driver's own
calls to SSM use that profile, so it needs `ssm:StartSession` and `ssm:TerminateSession` for the
target instance and the `AWS-StartPortForwardingSession` document, in addition to what
[`aws-iam.md`](aws-iam.md) already lists. (That file does not yet list these; add them.)

## 4. Prepare the target image (the AMI)

wrangl launches a target **with no user-data** (`LaunchTarget` sets none), so everything must be in
the AMI:

1. A KVM-capable instance family (section 2) and the instance profile above (the target is launched
   with the same profile, which must include `AmazonSSMManagedInstanceCore` so the SSM agent
   registers; `aws-iam.md` already says to attach it).
2. The Android SDK command-line tools, the emulator, a system image and an AVD, for example:
   `sdkmanager "emulator" "platform-tools" "system-images;android-34;google_apis;x86_64"` and
   `avdmanager create avd -n ci -k "system-images;android-34;google_apis;x86_64"`.
3. A **service that boots the emulator on instance start**, headless:
   ```ini
   # /etc/systemd/system/emulator.service
   [Unit]
   After=network-online.target
   [Service]
   User=ec2-user
   ExecStart=/opt/android/emulator/emulator -avd ci -no-window -no-audio -no-snapshot -gpu swiftshader_indirect -port 5554
   Restart=always
   [Install]
   WantedBy=multi-user.target
   ```
   `-port 5554` puts the console on 5554 and adb on **5555**, which is the template's `connection.port`.
   By default the emulator listens on loopback only, which is what the SSM port-forward connects to.
4. Test it from a shell on the instance: `adb devices` lists `emulator-5554`, and
   `adb shell getprop sys.boot_completed` prints `1` once booted.

Booting takes a while after launch and wrangl does not wait for it: the target is marked `ready`
as soon as EC2 reports the instance `running` (the default probe is the instance state; with `via:
ssm` there is no adb-level probe). The first tool calls after a cold launch can therefore fail until
the emulator is up; see section 6.

## 5. Install the template

```
wrangl template create -f docs/wrangl/templates/remote-android.yaml    # edit ami / instance_types / driver first
wrangl project edit PROJ --limit android=1                             # opt in (default is 0)
wrangl project edit PROJ --template android                            # or queue work whose template is android
wrangl target list
```
No secret is needed for adb (the `secret` field is required only for ssh). The template is rejected
if `driver_template` does not exist, is itself remote, or `connection.protocol` is not `ssh`/`adb`.
Because the example names `agents: [claude]`, an agent called `claude` must exist (`wrangl agent list`).

A queued run on this template takes a target session (`max_sessions`, 1 here) and a driver session
at once; the controller launches the target and a driver as needed within the caps.

## 6. What the agent can do

- `target_put {path, dest}`: `adb push` of a file or directory **inside the checkout** to a path on the
  device. `path` is relative to the checkout; anything outside it is refused.
- `target_exec {cmd, timeout_s?}`: `adb shell CMD` on the emulator (default timeout 600 s, max
  3600 s; output beyond 64 KB keeps the tail). The result is `{output, exit_code}`.

Each call first runs `adb connect 127.0.0.1:<local port>`, so there is no long-lived adb server to
babysit. If the emulator is not up, `adb` reports it in the output and a non-zero exit code: retry.
A boot wait for the agent to use first:

```
until [ "$(getprop sys.boot_completed)" = "1" ]; do sleep 3; done
```

If the tunnel cannot be opened when the run starts, the run fails with "target lost". Once running, a
dropped tunnel shows up as `adb` errors in a tool's output (the daemon restarts the SSM session with
backoff); only for ssh targets is an unreachable host mapped to "target lost" by the tool. Separately,
the controller fails the runs on a target as `target lost` if its instance is gone or it stays
unreachable for two minutes (`OfflineGrace`).

## 7. Smoke test: build an APK on the driver, install it on the emulator

Pick a repo with a debug Android build, link it to a project that has the cap set, and queue a task
like this (adjust the module path and package):

> Build the debug APK with `./gradlew assembleDebug` in the checkout. Wait for the emulator with
> `target_exec` (`until [ "$(getprop sys.boot_completed)" = "1" ]; do sleep 3; done`). Copy the APK
> with `target_put` (`path: app/build/outputs/apk/debug/app-debug.apk`, `dest: /data/local/tmp/app.apk`).
> Install it with `target_exec` (`pm install -r /data/local/tmp/app.apk`; expect `Success`), start it
> with `target_exec` (`monkey -p com.example.app -c android.intent.category.LAUNCHER 1`) and check
> `dumpsys activity activities | grep -c com.example.app` is non-zero. Report the commands and results.

```
wrangl task create PROJ --title "Smoke: build and install on the emulator" --prompt "...the text above..."
wrangl task queue SQID
wrangl target list           # the target goes launching -> ready, with 1/1 sessions while it runs
wrangl logs RUN -f --stream target    # each target_exec/target_put is logged as `$ cmd -> exit N`
```
If the install says `Success` and the activity check is non-zero, the chain works: the driver built
the APK, the SSM tunnel and adb reached the emulator, and the controller tracked the target. If the
run ends `failed` with "target lost", see `wrangl target list` (state `offline`/`terminated`).

## 8. Known gaps

- No adb-level health probe through the tunnel and no wait for the emulator to boot (section 4).
- The Runners page does not show targets yet (use `wrangl target list`), and there is no terminal
  into the emulator host.
- No Android-specific cost warning on launch; the opt-in cap and `idle_timeout` are the controls.
- The emulator is not reset between runs (the template's `connection.reset` command runs on the
  target at session end; for adb it is passed to `adb shell`, e.g. `pm uninstall com.example.app`).
- adb is unauthenticated: keep `via: ssm`; `via: direct` would expose it to the security group.
