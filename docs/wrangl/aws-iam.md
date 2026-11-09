# wrangl on AWS: IAM roles

The controller has **no AWS keys in its config**. It uses the AWS SDK's default credential
chain, which on its EC2 host resolves to the host's **instance role**. `wrangl fleet
teardown`, run from a laptop, uses whatever local credentials or `--profile` you give it.

## Controller instance role

Least privilege: it can launch instances only if they carry `wrangl:managed=true`, tag only
at launch, and terminate only instances tagged `wrangl:managed=true` (the controller also
re-checks its own `wrangl:controller` id on the live instance before every terminate).
Replace `ACCOUNT`, `REGION` and the runner role name.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "LaunchTaggedRunners",
      "Effect": "Allow",
      "Action": "ec2:RunInstances",
      "Resource": [
        "arn:aws:ec2:REGION:ACCOUNT:instance/*",
        "arn:aws:ec2:REGION:ACCOUNT:volume/*",
        "arn:aws:ec2:REGION:ACCOUNT:network-interface/*"
      ],
      "Condition": { "StringEquals": { "aws:RequestTag/wrangl:managed": "true" } }
    },
    {
      "Sid": "LaunchFromSubnetSgAmi",
      "Effect": "Allow",
      "Action": "ec2:RunInstances",
      "Resource": [
        "arn:aws:ec2:REGION:ACCOUNT:subnet/*",
        "arn:aws:ec2:REGION:ACCOUNT:security-group/*",
        "arn:aws:ec2:REGION::image/*"
      ]
    },
    {
      "Sid": "TagOnlyAtLaunch",
      "Effect": "Allow",
      "Action": "ec2:CreateTags",
      "Resource": "*",
      "Condition": { "StringEquals": { "ec2:CreateAction": "RunInstances" } }
    },
    {
      "Sid": "TerminateOnlyWranglInstances",
      "Effect": "Allow",
      "Action": "ec2:TerminateInstances",
      "Resource": "arn:aws:ec2:REGION:ACCOUNT:instance/*",
      "Condition": { "StringEquals": { "aws:ResourceTag/wrangl:managed": "true" } }
    },
    {
      "Sid": "Describe",
      "Effect": "Allow",
      "Action": ["ec2:DescribeInstances", "ec2:DescribeInstanceTypes", "ec2:DescribeImages"],
      "Resource": "*"
    },
    {
      "Sid": "ResolvePublicAmiParameters",
      "Effect": "Allow",
      "Action": "ssm:GetParameter",
      "Resource": "arn:aws:ssm:REGION::parameter/aws/service/ami-amazon-linux-latest/*"
    },
    {
      "Sid": "PassRunnerRole",
      "Effect": "Allow",
      "Action": "iam:PassRole",
      "Resource": "arn:aws:iam::ACCOUNT:role/wrangl-runner"
    }
  ]
}
```

### Optional: spend and other instances (Runners page)

The Runners page shows month-to-date spend, a forecast, hourly burn and the instances in the
account that wrangl did not launch (every region). Those are read-only calls that the policy above does not
grant. Without them the rest of wrangl works and the page shows a note instead of figures. The
first JSON block in this file is the one `deploy/wrangl/controller.sh` attaches; attach this one
yourself if you want the figures:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "SpendAndOtherInstances",
      "Effect": "Allow",
      "Action": [
        "ce:GetCostAndUsage",
        "ce:GetCostAndUsageWithResources",
        "ce:GetCostForecast",
        "pricing:GetProducts",
        "ec2:DescribeRegions"
      ],
      "Resource": "*"
    }
  ]
}
```

Cost Explorer must be enabled for the account, and per-resource data (Cost Explorer > Preferences)
for the "most expensive other resources" list. Cost Explorer bills a small fee per request, so the
controller caches and refreshes in the background (`controller/costs/costs.go`).

## Runner instance role (`wrangl-runner`)

Attach `AmazonSSMManagedInstanceCore` so `wrangl dev connect` and `wrangl fleet connect` work for debugging. Runners talk only to the
controller. Add model-provider access here if a template uses Bedrock.

A runner that is the **driver** for a remote target (a Mac or Android emulator host, see
[`android.md`](android.md) and [ADR 0002](adr/0002-remote-runners.md)) also needs the AWS CLI,
the Session Manager plugin and permission to open a port-forward to the target:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    { "Effect": "Allow", "Action": ["ssm:StartSession", "ssm:TerminateSession"],
      "Resource": ["arn:aws:ec2:REGION:ACCOUNT:instance/*",
                   "arn:aws:ssm:REGION::document/AWS-StartPortForwardingSession"] }
  ]
}
```

Scope the instance ARN to targets (for example with a `aws:ResourceTag/wrangl:managed` condition)
rather than leaving it open.

## Artifact bucket (only with `deploy/wrangl/controller.sh`)

The script also gives the controller role `s3:GetObject` on `arn:aws:s3:::<bucket>/wrangl/*`, the
private bucket the upgrade command uploads the three binaries to. It is not part of the policy
above.

## Network

Runners and the controller share a security group that references itself:

| Port | From | Purpose |
|---|---|---|
| 4222/tcp | runner SG | NATS (TLS, per-runner JWTs) |
| 4223/tcp | runner SG | enrolment + runner binary download |

Runners need outbound internet (mise, GitHub, model provider) through a NAT gateway. The
enrolment check compares the request's source IP with the instance's private IP, so runners
must reach the controller **directly inside the VPC** (no NAT or proxy in between).

## Teardown

Every instance, root volume and network interface is tagged `wrangl:managed=true`,
`wrangl:controller=<id>`, `wrangl:runner`, `wrangl:template` and `Name=wrangl-<runner>`.

```sh
# on the controller host (reads the controller id locally)
wrangl fleet teardown
# from anywhere, with your own credentials
wrangl fleet teardown --region us-west-2 --controller-id wr-230f447b13
wrangl fleet teardown --region us-west-2 --all-controllers --yes
```

Root volumes are `DeleteOnTermination`, so terminating the instances removes everything.
Even if nobody runs teardown, an orphaned runner powers itself off after 2 h without its
controller, and instances launch with `InstanceInitiatedShutdownBehavior=terminate`.
