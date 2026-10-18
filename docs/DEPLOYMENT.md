# Deploying a control node

This is the contract for running a schmeide control node anywhere. It does not
assume a cloud, an orchestrator, or a host. The [reference pattern](#reference-pattern-a-vm-running-the-container)
at the end is one way to meet it, not the only one. To try schmeide on one
machine first, follow [QUICKSTART.md](QUICKSTART.md).

## The image

Each `v*` tag publishes `ghcr.io/autobutler-org/schmeide:<version>`, with the
`v` dropped (tag `v0.0.1` is image `0.0.1`), and moves `latest` to it. See
[VERSIONING.md](VERSIONING.md).

Pin an exact version in production, not `latest`, and upgrade on purpose. The
control node warns when a runner reports a different semantic version, so keep
runners on the same release.

The image runs `schmeide` as its entrypoint, as the nonroot user (UID 65532).
It serves the control node only. Runner mode needs `git` and a harness CLI,
which the image does not have: run runners from a binary on the runner
machine (`make install`, or `go install` of a tag).

## Ports

| Port | Setting | Protocol | Who connects |
|---|---|---|---|
| 8080 | `SCHMEIDE_HTTP_ADDR` (default `:8080`) | plain HTTP | GitHub, through your TLS-terminating proxy |
| 4222 | `SCHMEIDE_NATS_ADDR` (default `127.0.0.1:4222`) | NATS, TLS when configured | Runners and `schmeide status` |

The webhook listener is plain HTTP. Never expose 8080 directly: put a
TLS-terminating proxy in front of it and expose only the proxy's 443.

NATS serves TLS itself. A TLS proxy cannot sit in front of it, because a NATS
client starts in plain text and upgrades to TLS after the server's greeting.
Set both:

- `SCHMEIDE_NATS_TLS_CERT_FILE`: the PEM certificate, with its chain.
- `SCHMEIDE_NATS_TLS_KEY_FILE`: the PEM private key.

With both set, NATS refuses plain connections. Setting only one is a startup
error. Both files are read again on every TLS handshake, so a renewed
certificate is served without a restart. If a read fails, for example while a
renewal is half-written, the last pair that loaded is served. A bad pair at
startup stops the control node.

Without TLS, the control node refuses to start when NATS listens beyond
loopback, because steward tokens and every job's GitHub token would then cross
the network in plain text. The error names `--nats-tls-cert-file` and
`--nats-tls-key-file`. On a network you trust, such as a private container
network with no other tenants, set `SCHMEIDE_ALLOW_PLAINTEXT_NATS=true`
(`--allow-plaintext-nats true`): the control node then starts and logs a
warning instead.

## Persistent storage

`SCHMEIDE_DATA_DIR` holds JetStream's files: the job queue, the per-issue
records that dedupe webhooks, the stage records a restarted runner resumes
from, and the status history `schmeide status` shows. Put it on persistent
block storage that outlives the machine or container. The image's user, UID
65532, must be able to write it. A fresh named volume already is; for a host
directory or disk, run `sudo chown 65532:65532 /data` on the host.

Losing it loses all of that. Queued jobs are gone, an issue that already had a
job can get a second one, and a runner cannot resume a job in progress. The
issues and pull requests on GitHub are unaffected; relabel an issue to queue
it again.

Backups:

- A block-storage snapshot is crash-consistent. JetStream recovers from it as
  it would from a power loss.
- For a clean copy, stop the container, copy or snapshot the directory, and
  start it again.
- Restore by putting the directory back before the control node starts. Jobs
  finished after the backup may be offered again; the per-issue records make
  most of those a no-op.

## Settings

Every setting is a flag, a `SCHMEIDE_*` environment variable, or a line in a
`--config` file. In a container, use environment variables and mounted files.
`schmeide control --help` is the full list.

Required:

| Variable | Value |
|---|---|
| `SCHMEIDE_WEBHOOK_SECRET` | The GitHub App's webhook secret. |
| `SCHMEIDE_GITHUB_APP_ID` | The GitHub App's ID. |
| `SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE` | Path of the App's private key. In the image, `/run/secrets/github-app.pem`. |
| `SCHMEIDE_RUNNER_TOKENS` | Comma-separated `<steward login>:<token>` entries, one per steward. |
| `SCHMEIDE_ALLOWED_ORGS` or `SCHMEIDE_ALLOWED_INSTALLATIONS` | At least one: comma-separated owner logins, or App installation IDs, whose repositories may create jobs. |

For a container:

| Variable | Value |
|---|---|
| `SCHMEIDE_NATS_ADDR` | `0.0.0.0:4222`. The default, `127.0.0.1:4222`, is unreachable from outside the container. |
| `SCHMEIDE_NATS_TLS_CERT_FILE`, `SCHMEIDE_NATS_TLS_KEY_FILE` | The NATS certificate and key, as above. Without them, `0.0.0.0:4222` is refused at startup unless `SCHMEIDE_ALLOW_PLAINTEXT_NATS=true`. |
| `SCHMEIDE_DATA_DIR` | The image sets `/data`. Mount persistent storage there. |

Optional:

| Variable | Default | Value |
|---|---|---|
| `SCHMEIDE_HTTP_ADDR` | `:8080` | Webhook listener address. |
| `SCHMEIDE_READY_LABEL` | `schmeide:ready` | Label that, with an assignee, makes an issue a job. |
| `SCHMEIDE_WEBHOOK_RATE` | `1000` | Webhook requests accepted per minute. |
| `SCHMEIDE_DELIVERY_TTL` | `10m` | How long a delivery id is remembered, to drop replays. |
| `SCHMEIDE_GITHUB_API_URL` | `https://api.github.com` | GitHub REST API base URL. |
| `SCHMEIDE_CONFIG` | none | Path of a config file, if you prefer one to variables. |

Keep secrets out of the process list and out of images: pass them from your
secret store as environment variables or mounted files.

## The private key file

The control node refuses a private key that group or other can read. The file
must be mode 0600 and readable by UID 65532, the image's user. A bind mount
keeps the host file's owner and mode, so set them on the host:

```sh
sudo chown 65532:65532 /etc/schmeide/github-app.pem
sudo chmod 0600 /etc/schmeide/github-app.pem
```

Mount it read-only at `/run/secrets/github-app.pem`. The NATS key file must be
readable by UID 65532 too.

## Connecting GitHub and runners

- **GitHub App webhook URL:** `https://<host>/webhook`, through the TLS proxy.
  Creating the App is in [QUICKSTART.md](QUICKSTART.md).
- **Runners:** `schmeide runner --steward <login> --nats-token <token>
  --nats-url tls://<host>:4222`. The certificate must be valid for `<host>`.
  Runners verify it against the system roots. For a private CA on Linux, point
  `SSL_CERT_FILE` at a bundle that includes it. Run `schmeide doctor` with the
  same flags first.

## Shutdown and restarts

On SIGTERM or SIGINT, the control node stops accepting webhooks, waits up to
10 seconds for requests in flight, then stops NATS. Give the container at
least that long to stop (`docker stop` waits 10 seconds by default; pass
`--stop-timeout 30` to `docker run` for margin).

While the control node is down, GitHub's deliveries fail and can be
redelivered from the App's settings, and runners keep reconnecting. A job a
runner holds is offered again if its heartbeat lapses, and the runner resumes
from its stage record.

## Reference pattern: a VM running the container

One VM runs the control node container and Caddy:

- A managed disk mounted at `/data` on the host and passed to the container at
  `/data`.
- Caddy on the host, or in its own container, reverse-proxies 443 to
  `127.0.0.1:8080` and gets the certificate from an ACME CA.
- NATS on 4222 serves the same certificate. It reads the files from Caddy's
  storage, mounted read-only, and picks up each renewal on the next handshake.
- The firewall opens 443 and 4222 only.

Caddyfile:

```
schmeide.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

Caddy keeps the certificate and key under its data directory, at
`certificates/<issuer>/<host>/<host>.crt` and `<host>.key`. The key must be
readable by UID 65532, so run Caddy as that user or make the key group-readable
to it. Then:

```sh
docker run -d --name schmeide --restart unless-stopped --stop-timeout 30 \
  -p 127.0.0.1:8080:8080 -p 4222:4222 \
  -v /data:/data \
  -v /etc/schmeide/github-app.pem:/run/secrets/github-app.pem:ro \
  -v /var/lib/caddy/certificates/acme-v02.api.letsencrypt.org-directory/schmeide.example.com:/run/tls:ro \
  -e SCHMEIDE_NATS_ADDR=0.0.0.0:4222 \
  -e SCHMEIDE_NATS_TLS_CERT_FILE=/run/tls/schmeide.example.com.crt \
  -e SCHMEIDE_NATS_TLS_KEY_FILE=/run/tls/schmeide.example.com.key \
  -e SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/github-app.pem \
  --env-file /etc/schmeide/control.env \
  ghcr.io/autobutler-org/schmeide:0.0.1
```

`/etc/schmeide/control.env` holds `SCHMEIDE_WEBHOOK_SECRET`,
`SCHMEIDE_GITHUB_APP_ID`, `SCHMEIDE_RUNNER_TOKENS`, and
`SCHMEIDE_ALLOWED_ORGS`, mode 0600.

As an example only, the autobutler control node, schmeide.cloud.autobutler.org,
follows this pattern and is defined in autobutler-org/iac. There, Caddy runs
as UID 65532 and its certificate directory for the host is mounted read-only at
`/run/tls`. Nothing in schmeide depends on that repository.
