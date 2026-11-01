#!/usr/bin/env bash
# wrangl controller on AWS: INSTALL.md sections 3-5 as one idempotent script.
#
#   deploy/wrangl/controller.sh up        # SG, IAM, artifact bucket, controller instance
#   deploy/wrangl/controller.sh upgrade   # new binaries to the bucket; reinstall + restart over SSM
#   deploy/wrangl/controller.sh status    # what exists
#   deploy/wrangl/controller.sh down      # terminate runners + controller, delete what `up` made
#
# Run from the repo root on a machine with AWS credentials that may manage EC2, IAM, S3
# and SSM (an admin profile is simplest). Required environment:
#   AWS_REGION   e.g. us-west-2
#   VPC_ID       the VPC
#   SUBNET_ID    a PRIVATE subnet whose route table sends 0.0.0.0/0 to a NAT gateway
# Optional:
#   INSTANCE_TYPE (t4g.small)   BUILD_DIR (build)   NAME (wrangl)
#
# The script never creates a VPC or NAT gateway (they cost money and you may have one).
# Everything it creates is named "$NAME..." and found again by that name, so re-running
# `up` is safe. The controller's own user data installs wrangl, writes its config and
# starts it under systemd; progress is in /var/log/wrangl-setup.log on the instance.
set -euo pipefail

NAME=${NAME:-wrangl}
INSTANCE_TYPE=${INSTANCE_TYPE:-t4g.small}
BUILD_DIR=${BUILD_DIR:-build}
REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)

say()  { printf '\033[1m==>\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }
need() { [[ -n "${!1:-}" ]] || die "set $1 (see the header of $0)"; }
aws() { command aws --region "$AWS_REGION" "$@"; }

need AWS_REGION
command -v aws >/dev/null || die "the aws CLI is required"
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
BUCKET="${NAME}-artifacts-${ACCOUNT}-${AWS_REGION}"
CTRL_ROLE="${NAME}-controller"
RUNNER_ROLE="${NAME}-runner"
CTRL_TAG="${NAME}-controller"

controller_id() { # the running/pending/stopped controller instance, if any
  aws ec2 describe-instances \
    --filters "Name=tag:Name,Values=$CTRL_TAG" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
    --query 'Reservations[].Instances[0].InstanceId' --output text | awk '{print $1}' | grep -v '^None$' || true
}

# --- network ------------------------------------------------------------------------

check_subnet() {
  need VPC_ID; need SUBNET_ID
  local vpc
  vpc=$(aws ec2 describe-subnets --subnet-ids "$SUBNET_ID" --query 'Subnets[0].VpcId' --output text)
  [[ "$vpc" == "$VPC_ID" ]] || die "subnet $SUBNET_ID is in $vpc, not $VPC_ID"
  # The subnet's route table (explicit association, else the VPC's main table).
  local rt
  rt=$(aws ec2 describe-route-tables --filters "Name=association.subnet-id,Values=$SUBNET_ID" \
        --query 'RouteTables[0].RouteTableId' --output text)
  if [[ "$rt" == "None" ]]; then
    rt=$(aws ec2 describe-route-tables --filters "Name=vpc-id,Values=$VPC_ID" "Name=association.main,Values=true" \
          --query 'RouteTables[0].RouteTableId' --output text)
  fi
  local nat igw
  nat=$(aws ec2 describe-route-tables --route-table-ids "$rt" \
         --query "RouteTables[0].Routes[?DestinationCidrBlock=='0.0.0.0/0'].NatGatewayId" --output text)
  igw=$(aws ec2 describe-route-tables --route-table-ids "$rt" \
         --query "RouteTables[0].Routes[?DestinationCidrBlock=='0.0.0.0/0'].GatewayId" --output text)
  if [[ "$nat" == nat-* ]]; then
    say "subnet $SUBNET_ID is private with NAT ($nat)"
  elif [[ "$igw" == igw-* ]]; then
    warn "subnet $SUBNET_ID routes to an internet gateway (public). Instances here have no outbound internet unless they get public IPs; wrangl expects a private subnet with NAT."
  else
    die "subnet $SUBNET_ID has no default route: runners need outbound internet (mise, GitHub, model APIs) through a NAT gateway"
  fi
}

ensure_sg() {
  SG_ID=$(aws ec2 describe-security-groups --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=$NAME" \
           --query 'SecurityGroups[0].GroupId' --output text)
  if [[ "$SG_ID" == "None" ]]; then
    SG_ID=$(aws ec2 create-security-group --group-name "$NAME" --description "wrangl controller and runners" \
             --vpc-id "$VPC_ID" --query GroupId --output text)
    say "created security group $SG_ID"
  else
    say "security group $SG_ID exists"
  fi
  # Members reach each other on 4222 (NATS) and 4223 (enrol + binary download). No other ingress.
  if ! out=$(aws ec2 authorize-security-group-ingress --group-id "$SG_ID" \
      --ip-permissions "IpProtocol=tcp,FromPort=4222,ToPort=4223,UserIdGroupPairs=[{GroupId=$SG_ID}]" 2>&1); then
    [[ "$out" == *InvalidPermission.Duplicate* ]] || die "$out"
  fi
}

# --- IAM ----------------------------------------------------------------------------

TRUST='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}'

ensure_role() { # role name; also its instance profile of the same name
  local role=$1
  if ! aws iam get-role --role-name "$role" >/dev/null 2>&1; then
    aws iam create-role --role-name "$role" --assume-role-policy-document "$TRUST" >/dev/null
    say "created role $role"
  fi
  aws iam attach-role-policy --role-name "$role" --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore
  if ! aws iam get-instance-profile --instance-profile-name "$role" >/dev/null 2>&1; then
    aws iam create-instance-profile --instance-profile-name "$role" >/dev/null
    aws iam add-role-to-instance-profile --instance-profile-name "$role" --role-name "$role"
    say "created instance profile $role"
    NEW_PROFILE=1
  fi
}

controller_policy() { # the least-privilege policy, taken verbatim from docs/wrangl/aws-iam.md
  awk '/^```json$/{f=1;next} /^```$/{if(f)exit} f' "$REPO_ROOT/docs/wrangl/aws-iam.md" |
    sed "s/REGION/$AWS_REGION/g; s/ACCOUNT/$ACCOUNT/g; s#role/wrangl-runner#role/$RUNNER_ROLE#g"
}

ensure_iam() {
  NEW_PROFILE=0
  ensure_role "$RUNNER_ROLE"
  ensure_role "$CTRL_ROLE"
  local policy
  policy=$(controller_policy)
  [[ "$policy" == *TerminateOnlyWranglInstances* ]] || die "could not read the controller policy from docs/wrangl/aws-iam.md"
  aws iam put-role-policy --role-name "$CTRL_ROLE" --policy-name "$NAME-controller" --policy-document "$policy"
  # Read the release binaries from the artifact bucket (and nothing else in S3).
  aws iam put-role-policy --role-name "$CTRL_ROLE" --policy-name "$NAME-artifacts" --policy-document \
    "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Effect\":\"Allow\",\"Action\":\"s3:GetObject\",\"Resource\":\"arn:aws:s3:::$BUCKET/wrangl/*\"}]}"
  say "IAM roles ready ($CTRL_ROLE, $RUNNER_ROLE)"
}

# --- artifacts ----------------------------------------------------------------------

ensure_bucket() {
  if ! aws s3api head-bucket --bucket "$BUCKET" >/dev/null 2>&1; then
    if [[ "$AWS_REGION" == us-east-1 ]]; then
      aws s3api create-bucket --bucket "$BUCKET" >/dev/null
    else
      aws s3api create-bucket --bucket "$BUCKET" --create-bucket-configuration "LocationConstraint=$AWS_REGION" >/dev/null
    fi
    say "created bucket $BUCKET"
  fi
  aws s3api put-public-access-block --bucket "$BUCKET" --public-access-block-configuration \
    BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
  aws s3api put-bucket-encryption --bucket "$BUCKET" --server-side-encryption-configuration \
    '{"Rules":[{"ApplyServerSideEncryptionByDefault":{"SSEAlgorithm":"AES256"}}]}'
}

sha() { sha256sum "$1" | awk '{print $1}'; }

upload_binaries() {
  for a in aarch64 x86_64; do
    [[ -f "$BUILD_DIR/wrangl-linux-$a" ]] || die "$BUILD_DIR/wrangl-linux-$a missing: run \`task build-wrangl\` first"
  done
  SUM_AARCH64=$(sha "$BUILD_DIR/wrangl-linux-aarch64")
  SUM_X86_64=$(sha "$BUILD_DIR/wrangl-linux-x86_64")
  for a in aarch64 x86_64; do
    aws s3 cp --only-show-errors "$BUILD_DIR/wrangl-linux-$a" "s3://$BUCKET/wrangl/wrangl-linux-$a"
  done
  say "uploaded binaries (aarch64 ${SUM_AARCH64:0:12}, x86_64 ${SUM_X86_64:0:12})"
}

# The install steps, shared by first boot (user data) and `upgrade` (SSM).
install_script() {
  cat <<EOF
set -euo pipefail
for a in aarch64 x86_64; do
  aws s3 cp --only-show-errors --region $AWS_REGION s3://$BUCKET/wrangl/wrangl-linux-\$a /tmp/wrangl-linux-\$a
done
echo "$SUM_AARCH64  /tmp/wrangl-linux-aarch64" | sha256sum -c -
echo "$SUM_X86_64  /tmp/wrangl-linux-x86_64" | sha256sum -c -
id wrangl >/dev/null 2>&1 || useradd --system --home-dir /var/lib/wrangl --shell /sbin/nologin wrangl
install -d -o wrangl -g wrangl -m 0750 /etc/wrangl /var/lib/wrangl /var/lib/wrangl/bin
install -m 0755 /tmp/wrangl-linux-\$(uname -m) /usr/local/bin/wrangl
# Served to runners at boot and for self-upgrade.
install -o wrangl -g wrangl -m 0755 /tmp/wrangl-linux-aarch64 /tmp/wrangl-linux-x86_64 /var/lib/wrangl/bin/
rm -f /tmp/wrangl-linux-aarch64 /tmp/wrangl-linux-x86_64
EOF
}

user_data() {
  cat <<EOF
#!/bin/bash
# wrangl controller first boot (generated by deploy/wrangl/controller.sh)
exec >>/var/log/wrangl-setup.log 2>&1
set -euo pipefail
echo "wrangl setup starting \$(date -u)"
$(install_script)
usermod -aG wrangl ec2-user || true
TOKEN=\$(curl -fsS -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
IP=\$(curl -fsS -H "X-aws-ec2-metadata-token: \$TOKEN" http://169.254.169.254/latest/meta-data/local-ipv4)
if [ ! -f /etc/wrangl/controller.yaml ]; then
  cat > /etc/wrangl/controller.yaml <<CFG
# wrangl controller config (written at first boot; see docs/wrangl/INSTALL.md 5.4)
data_dir: /var/lib/wrangl
listen:
  http: 127.0.0.1:8080
  unix: /run/wrangl/api.sock
  nats: \$IP:4222
  enroll: \$IP:4223
aws:
  region: $AWS_REGION
  subnet_ids: [$SUBNET_ID]
  security_group_id: $SG_ID
  runner_instance_profile: $RUNNER_ROLE
github:
  author_app: { id: 0, private_key: secrets/gh-author.pem }
  reviewer_app: { id: 0, private_key: secrets/gh-reviewer.pem, webhook_secret: secrets/gh-webhook }
cloudflare: { team: "", aud: "", owner_email: "" }
web: { public_url: "" }
CFG
  chown wrangl:wrangl /etc/wrangl/controller.yaml
  chmod 0640 /etc/wrangl/controller.yaml
fi
sudo -u wrangl /usr/local/bin/wrangl --config /etc/wrangl/controller.yaml controller init --data-dir /var/lib/wrangl
cat > /etc/systemd/system/wrangl-controller.service <<UNIT
[Unit]
Description=wrangl controller
After=network-online.target
Wants=network-online.target

[Service]
User=wrangl
Group=wrangl
ExecStart=/usr/local/bin/wrangl --config /etc/wrangl/controller.yaml controller serve
Restart=always
RestartSec=5
RuntimeDirectory=wrangl
RuntimeDirectoryMode=0750

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now wrangl-controller
echo "wrangl setup done \$(date -u)"
EOF
}

launch_controller() {
  local id
  id=$(controller_id)
  if [[ -n "$id" ]]; then
    say "controller instance $id exists (use \`upgrade\` to change its binaries)"
    CTRL_ID=$id
    return
  fi
  local arch
  arch=$(aws ec2 describe-instance-types --instance-types "$INSTANCE_TYPE" \
          --query 'InstanceTypes[0].ProcessorInfo.SupportedArchitectures[0]' --output text)
  [[ "$arch" == arm64 || "$arch" == x86_64 ]] || die "instance type $INSTANCE_TYPE: unsupported architecture $arch"
  local ud
  ud=$(mktemp)
  user_data >"$ud"
  # A new instance profile takes a few seconds to become usable by EC2: retry.
  for i in 1 2 3 4 5 6; do
    if CTRL_ID=$(aws ec2 run-instances \
        --image-id "resolve:ssm:/aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-$arch" \
        --instance-type "$INSTANCE_TYPE" --subnet-id "$SUBNET_ID" --security-group-ids "$SG_ID" \
        --iam-instance-profile "Name=$CTRL_ROLE" \
        --metadata-options HttpTokens=required,HttpEndpoint=enabled \
        --block-device-mappings '[{"DeviceName":"/dev/xvda","Ebs":{"VolumeSize":30,"VolumeType":"gp3","Encrypted":true}}]' \
        --tag-specifications "ResourceType=instance,Tags=[{Key=Name,Value=$CTRL_TAG}]" \
        --user-data "file://$ud" --query 'Instances[0].InstanceId' --output text 2>"$ud.err"); then
      break
    fi
    grep -q 'Invalid IAM Instance Profile' "$ud.err" || { cat "$ud.err" >&2; rm -f "$ud" "$ud.err"; exit 1; }
    say "instance profile not ready yet; retrying ($i)"
    sleep $((i * 5))
  done
  rm -f "$ud" "$ud.err"
  [[ "$CTRL_ID" == i-* ]] || die "could not launch the controller"
  say "launched controller $CTRL_ID ($INSTANCE_TYPE, $arch); waiting for it to run"
  aws ec2 wait instance-running --instance-ids "$CTRL_ID"
}

# --- commands -----------------------------------------------------------------------

cmd_up() {
  check_subnet
  ensure_sg
  ensure_iam
  ensure_bucket
  upload_binaries
  launch_controller
  local ip
  ip=$(aws ec2 describe-instances --instance-ids "$CTRL_ID" --query 'Reservations[0].Instances[0].PrivateIpAddress' --output text)
  cat >&2 <<EOF

Controller $CTRL_ID at $ip. First boot installs and starts wrangl (about a minute).

  aws ssm start-session --region $AWS_REGION --target $CTRL_ID
  sudo tail -f /var/log/wrangl-setup.log          # until "wrangl setup done"
  sudo journalctl -u wrangl-controller -n 20      # "controller serving ... aws=true"
  curl --unix-socket /run/wrangl/api.sock http://x/healthz

Next: INSTALL.md section 6 (GitHub App), 7 (Claude token), 8 (template + first runner).
EOF
}

cmd_upgrade() {
  CTRL_ID=$(controller_id)
  [[ -n "$CTRL_ID" ]] || die "no controller instance named $CTRL_TAG"
  ensure_bucket
  upload_binaries
  local tmp cmd
  tmp=$(mktemp)
  { install_script; printf 'systemctl restart wrangl-controller\nsleep 3\nsystemctl is-active wrangl-controller\n'; } >"$tmp"
  aws s3 cp --only-show-errors "$tmp" "s3://$BUCKET/wrangl/upgrade.sh"
  rm -f "$tmp"
  # The controller role may read s3://$BUCKET/wrangl/* only; the script carries the sha256s.
  cmd=$(aws ssm send-command --instance-ids "$CTRL_ID" --document-name AWS-RunShellScript --comment "wrangl upgrade" \
         --parameters "commands=[\"aws s3 cp --only-show-errors --region $AWS_REGION s3://$BUCKET/wrangl/upgrade.sh /tmp/wrangl-upgrade.sh && bash /tmp/wrangl-upgrade.sh\"]" \
         --query Command.CommandId --output text)
  say "upgrade sent ($cmd); waiting"
  aws ssm wait command-executed --command-id "$cmd" --instance-id "$CTRL_ID" || true
  aws ssm get-command-invocation --command-id "$cmd" --instance-id "$CTRL_ID" \
    --query '[Status,StandardOutputContent,StandardErrorContent]' --output text
  say "runners on the old binary upgrade themselves as they go idle (wrangl fleet list shows \"(upgrading)\")"
}

cmd_status() {
  local id
  id=$(controller_id)
  echo "account     $ACCOUNT ($AWS_REGION)"
  echo "controller  ${id:-none}"
  [[ -n "$id" ]] && aws ec2 describe-instances --instance-ids "$id" \
    --query 'Reservations[0].Instances[0].[State.Name,InstanceType,PrivateIpAddress]' --output text | sed 's/^/            /'
  echo "runners     $(aws ec2 describe-instances --filters Name=tag:wrangl:managed,Values=true Name=instance-state-name,Values=pending,running \
                       --query 'length(Reservations[].Instances[])' --output text)"
  echo "bucket      $(aws s3api head-bucket --bucket "$BUCKET" >/dev/null 2>&1 && echo "$BUCKET" || echo none)"
  for r in "$CTRL_ROLE" "$RUNNER_ROLE"; do
    echo "role        $r: $(aws iam get-role --role-name "$r" >/dev/null 2>&1 && echo present || echo none)"
  done
}

cmd_down() {
  local id
  id=$(controller_id)
  echo "This terminates EVERY wrangl-managed runner in $AWS_REGION, the controller ${id:-(none)},"
  echo "and deletes the $NAME security group, IAM roles and the $BUCKET bucket."
  echo "The controller's database dies with its volume: back it up first if you need it."
  read -r -p "Type the region to confirm: " answer
  [[ "$answer" == "$AWS_REGION" ]] || die "not confirmed"
  local runners
  runners=$(aws ec2 describe-instances --filters Name=tag:wrangl:managed,Values=true \
             Name=instance-state-name,Values=pending,running,stopping,stopped \
             --query 'Reservations[].Instances[].InstanceId' --output text)
  local all="$runners ${id:-}"
  if [[ -n "${all// /}" ]]; then
    # shellcheck disable=SC2086
    aws ec2 terminate-instances --instance-ids $all >/dev/null
    say "terminating $all"
    # shellcheck disable=SC2086
    aws ec2 wait instance-terminated --instance-ids $all
  fi
  need VPC_ID
  local sg
  sg=$(aws ec2 describe-security-groups --filters "Name=vpc-id,Values=$VPC_ID" "Name=group-name,Values=$NAME" \
        --query 'SecurityGroups[0].GroupId' --output text)
  [[ "$sg" != "None" ]] && aws ec2 delete-security-group --group-id "$sg" && say "deleted $sg"
  for r in "$CTRL_ROLE" "$RUNNER_ROLE"; do
    aws iam remove-role-from-instance-profile --instance-profile-name "$r" --role-name "$r" 2>/dev/null || true
    aws iam delete-instance-profile --instance-profile-name "$r" 2>/dev/null || true
    for p in $(aws iam list-role-policies --role-name "$r" --query PolicyNames --output text 2>/dev/null); do
      aws iam delete-role-policy --role-name "$r" --policy-name "$p"
    done
    aws iam detach-role-policy --role-name "$r" --policy-arn arn:aws:iam::aws:policy/AmazonSSMManagedInstanceCore 2>/dev/null || true
    aws iam delete-role --role-name "$r" 2>/dev/null && say "deleted role $r" || true
  done
  if aws s3api head-bucket --bucket "$BUCKET" >/dev/null 2>&1; then
    aws s3 rm --only-show-errors "s3://$BUCKET" --recursive
    aws s3api delete-bucket --bucket "$BUCKET"
    say "deleted bucket $BUCKET"
  fi
}

case "${1:-}" in
  up) cmd_up ;;
  upgrade) cmd_upgrade ;;
  status) cmd_status ;;
  down) cmd_down ;;
  *) sed -n '2,8p' "$0"; exit 2 ;;
esac
