# Caddy Trusted CloudFront Origin Prefix List (IP Source)

A Caddy v2 module that resolves **AWS Managed Prefix Lists** for CloudFront’s *origin-facing* networks and feeds them to `trusted_proxies`. Use this when your Caddy sits behind Amazon CloudFront and you want accurate, auto-refreshed CIDR ranges for real client IP extraction.

> Module ID: `http.ip_sources.cloudfront_origin_pl`

---

## Features

* 🔁 Periodically pulls CIDRs from EC2 Managed Prefix Lists (default every **12h**)
* 🔎 Resolve by **prefix list ID** *or* by **name** (defaults to CloudFront origin-facing)
* 🌐 Optional **IPv6** support (automatic when `include_ipv6` is true)
* 🔐 Minimal IAM permissions required (read-only EC2 prefix list APIs)
* 🔗 Flexible AWS auth: env vars / shared profile / **assume-role**

---

## Quick start

### 1) Build a Caddy with this module

Using [`xcaddy`](https://github.com/caddyserver/xcaddy):

```bash
xcaddy build \
  --with github.com/NebulaRover77/cf-origin-pl
```

Or use the provided smoke image (Docker):

```bash
make smoke
# then run it:
docker run --rm -p 8080:8080 \
  -v "$PWD/Caddyfile.example:/etc/caddy/Caddyfile:ro" \
  -e AWS_REGION=us-east-1 \
  cf-origin-pl-smoke:local caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
```

> For CI/build-only checks: `make docker-ci` runs `go vet`, `go build`, and `go test` inside a container.

### 2) Configure Caddy

**Caddyfile example** (included as `Caddyfile.example`):

```caddy
{
  servers {
    trusted_proxies cloudfront_origin_pl {
      region us-east-1
      # one of:
      # prefix_list_id pl-3b927c52
      prefix_list_name com.amazonaws.global.cloudfront.origin-facing

      # Set true only if the IPv6 prefix list exists for you
      include_ipv6 false

      refresh 12h
      # aws_profile prod
      # role_arn arn:aws:iam::123:role/assume-me
    }

    client_ip_headers X-Forwarded-For CloudFront-Viewer-Address
    trusted_proxies_strict
  }
}
```

---

## How it works

* On startup, the module resolves one or two **Managed Prefix Lists**:

  * IPv4 name default: `com.amazonaws.global.cloudfront.origin-facing`
  * IPv6 name default: `com.amazonaws.global.cloudfront.origin-facing-ipv6` (enabled when `include_ipv6: true`)
* It fetches all CIDR entries (with pagination), deduplicates, and exposes them to Caddy’s `trusted_proxies`.
* A background ticker refreshes the list every `refresh` (default **12h**). Previous good lists are kept if a refresh fails.

### Region resolution

1. Value from `region` in config, else
2. `AWS_REGION` or `AWS_DEFAULT_REGION`, else
3. Default `us-east-1`

> For CloudFront’s **global** prefix list names, `us-east-1` generally works; you can still point to another region if your PLs live there.

---

## AWS authentication

This module uses the standard AWS SDK v2 config chain:

* Environment (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`)
* Shared config/credentials files with `aws_profile` (e.g., `AWS_PROFILE=prod`)
* Assume role with `role_arn` (STS) if provided

### Minimal IAM policy

`iam_permissions.json` is bundled; it effectively requires:

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "ec2:DescribeManagedPrefixLists",
      "ec2:GetManagedPrefixListEntries"
    ],
    "Resource": "*"
  }]
}
```

Attach this to the identity/role used by Caddy.

---

## Configuration reference (Caddyfile block)

Inside `trusted_proxies { source cloudfront_origin_pl { ... } }`:

| Option             | Type     | Default                                         | Notes                                                                                |
| ------------------ | -------- | ----------------------------------------------- | ------------------------------------------------------------------------------------ |
| `region`           | string   | `us-east-1` (if not set via env)                | AWS Region to query for the managed prefix lists                                     |
| `prefix_list_id`   | string   | *(empty)*                                       | Use this **or** `prefix_list_name`                                                   |
| `prefix_list_name` | string   | `com.amazonaws.global.cloudfront.origin-facing` | Name for IPv4 list; IPv6 defaults to the `-ipv6` variant when `include_ipv6` is true |
| `include_ipv6`     | bool     | `false`                                         | If `true`, also includes IPv6 list entries                                           |
| `refresh`          | duration | `12h`                                           | How often to refresh the list                                                        |
| `aws_profile`      | string   | *(empty)*                                       | Optional shared config profile name                                                  |
| `role_arn`         | string   | *(empty)*                                       | Optional role to assume via STS                                                      |
| `require_nonempty` | bool     | `false`                                         | If `true`, fail startup/refresh when the final prefix set is empty                   |

> Provide **either** `prefix_list_id` **or** `prefix_list_name`. If neither is set, the default IPv4 name is used; IPv6 name is added only when `include_ipv6` is true.

---

## Development

### Requirements

* Go **1.25+** (Caddy v2.10.x requires Go ≥ 1.25)
* Docker (optional, for containerized dev)

### Common tasks

```bash
# Build & run tests in Docker
make docker-ci

# Build a Caddy image with your module and verify it’s registered
make smoke

# Clean images
make clean
```

### Local `xcaddy` build

```bash
GOFLAGS=-buildvcs=false xcaddy build \
  --with github.com/NebulaRover77/cf-origin-pl
```

---

## Troubleshooting

* **"managed prefix list "…" not found in region …"**
  Check `region` and the exact PL name/ID. Remember IPv6 uses `…-ipv6`.

* **"resolved zero prefixes"**
  Usually IAM or wrong list name. Verify the policy and that the PL has entries.

* **AccessDenied on EC2 APIs**
  Attach the minimal IAM policy (see above) or adjust your role/assume-role chain.

* **Client IPs not restored**
  Ensure `client_ip_headers X-Forwarded-For CloudFront-Viewer-Address` and `trusted_proxies_strict` are set; confirm requests actually arrive via CloudFront.

---

## Roadmap / ideas

* Health metrics / counters (refresh success/fail, last updated)
* Optional static fallback CIDRs
* Caddy admin endpoint to force refresh

---

## Acknowledgements

* Built for Caddy v2’s `trusted_proxies` IP range source extension point
* Uses AWS SDK for Go v2
