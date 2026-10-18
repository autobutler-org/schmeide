# Operations journeys

Covers running a control node for real: the container image, NATS over TLS,
restarts, version skew, and logs. The contract is
[../DEPLOYMENT.md](../DEPLOYMENT.md). Versions are in
[../VERSIONING.md](../VERSIONING.md). Implemented in `internal/control`
(`control.go`, `tls.go`, `queue.go`, `status.go`), `internal/release`, the
`Dockerfile`, and `.github/workflows/release.yml`.

---

### JN-OPS-001: Pull a tagged image and check its version

**Preconditions:** A `v*` tag was pushed, for example `v0.0.1`, and the
release workflow finished.

**Steps:**

1. Run `docker pull ghcr.io/autobutler-org/schmeide:0.0.1`.
2. Run `docker run --rm ghcr.io/autobutler-org/schmeide:0.0.1 version`.

**Expected result:**

- The image exists for linux/amd64 and linux/arm64. `latest` points at the
  same image.
- Step 2 prints `v0.0.1`.

**Notes:**

- The image tag drops the `v`. The version stamp keeps it.
- Pin an exact version in production, not `latest`.
- The image runs the control node only. It has no git and no harness, so
  runners run from a binary.
- No automated test of the published image. Every pull request builds the
  image without pushing it (`.github/workflows/image.yml`).

---

### JN-OPS-002: Run the control node container

**Preconditions:** JN-SETUP-001 done. A host with Docker, a persistent disk at
`/data`, the App key at `/etc/schmeide/github-app.pem`, and
`/etc/schmeide/control.env` holding `SCHMEIDE_WEBHOOK_SECRET`,
`SCHMEIDE_GITHUB_APP_ID`, `SCHMEIDE_RUNNER_TOKENS`, and
`SCHMEIDE_ALLOWED_ORGS` (mode 0600). A TLS proxy forwards 443 to
`127.0.0.1:8080`. The host's certificate and key for NATS are at
`/etc/schmeide/tls/fullchain.pem` and `privkey.pem`, readable by UID 65532.

**Steps:**

1. Run `sudo chown 65532:65532 /data /etc/schmeide/github-app.pem` and
   `sudo chmod 0600 /etc/schmeide/github-app.pem`.
2. Run:

   ```sh
   docker run -d --name schmeide --restart unless-stopped --stop-timeout 30 \
     -p 127.0.0.1:8080:8080 -p 4222:4222 \
     -v /data:/data \
     -v /etc/schmeide/github-app.pem:/run/secrets/github-app.pem:ro \
     -e SCHMEIDE_NATS_ADDR=0.0.0.0:4222 \
     -e SCHMEIDE_NATS_TLS_CERT_FILE=/run/tls/fullchain.pem \
     -e SCHMEIDE_NATS_TLS_KEY_FILE=/run/tls/privkey.pem \
     -v /etc/schmeide/tls:/run/tls:ro \
     -e SCHMEIDE_GITHUB_APP_PRIVATE_KEY_FILE=/run/secrets/github-app.pem \
     --env-file /etc/schmeide/control.env \
     ghcr.io/autobutler-org/schmeide:0.0.1
   ```

3. Run `docker logs schmeide`.

**Expected result:**

- The log shows `msg="control node started"` with
  `webhook=http://[::]:8080/webhook`, `nats=0.0.0.0:4222`, and
  `nats_tls=true`.
- Without the two TLS variables the container exits at once with the
  plain-text NATS refusal (JN-OPS-005).
- Without TLS settings it also shows the plain-text warning (JN-OPS-005). Add
  TLS (JN-OPS-003) before runners connect from other machines.
- JetStream files appear under `/data`.

**Notes:**

- The image runs as UID 65532 and sets `SCHMEIDE_DATA_DIR=/data`. Port 8080
  is the plain HTTP webhook listener: expose it only to the proxy. Port 4222
  is NATS.
- A key file UID 65532 cannot read fails at start with "GitHub App private
  key: ...". A key with group or other read bits fails as JN-SETUP-008.
- No automated test.

---

### JN-OPS-003: Serve NATS over TLS for remote runners

**Preconditions:** JN-OPS-002. A certificate and key valid for the host's
name, readable by UID 65532.

**Steps:**

1. Mount the certificate directory read-only, for example at `/run/tls`, and
   set `SCHMEIDE_NATS_TLS_CERT_FILE=/run/tls/<host>.crt` and
   `SCHMEIDE_NATS_TLS_KEY_FILE=/run/tls/<host>.key`.
2. Restart the container.
3. On a runner machine, run `schmeide doctor --steward <login> --nats-token
   <token> --nats-url tls://<host>:4222`, then start the runner with the same
   flags.

**Expected result:**

- The startup log shows `nats_tls=true` and no plain-text warning.
- Doctor shows `ok  control node: reachable at tls://<host>:4222`. The runner
  registers.
- A client that does not use TLS cannot connect.

**Notes:**

- Runners verify the certificate against the system roots. For a private CA
  on Linux, point `SSL_CERT_FILE` at a bundle that includes it.
- A TLS proxy cannot sit in front of NATS. NATS serves TLS itself.
- Covered by `TestQueueServesTLS`.

---

### JN-OPS-004: Renew the NATS certificate without a restart

**Preconditions:** JN-OPS-003. An ACME client (for example Caddy) renews the
certificate files in place.

**Steps:**

1. Let the certificate renew, or replace both files by hand.
2. Start a new runner, or let an existing one reconnect.

**Expected result:**

- The new connection is served the renewed certificate. The control node was
  not restarted.
- If a read fails in the middle of a renewal (for example a half-written
  file), the last pair that loaded is served.

**Notes:**

- Both files are read on every TLS handshake. Connections already open keep
  their session.
- Covered by `TestQueueServesARotatedCertificate` and
  `TestQueueKeepsTheLastGoodCertificate`.

---

### JN-OPS-005: Control node refuses plain-text NATS beyond loopback

**Preconditions:** None.

**Steps:**

1. Start the control node with `--nats-addr 0.0.0.0:4222` (or `:4222`, or a
   LAN address) and no TLS files.
2. Start it again with the same address and `--allow-plaintext-nats true`
   (or `SCHMEIDE_ALLOW_PLAINTEXT_NATS=true`).
3. Start it with `--allow-plaintext-nats maybe`.

**Expected result:**

- Step 1: exit code 1, "schmeide control: --nats-addr 0.0.0.0:4222: NATS
  would listen beyond loopback without TLS, so steward tokens and every job's
  GitHub token would cross the network in plain text; set
  --nats-tls-cert-file and --nats-tls-key-file, listen on loopback
  (127.0.0.1:4222), or pass --allow-plaintext-nats true to accept the risk".
  Nothing is written to the data directory.
- Step 2: the control node starts and logs `level=WARN msg="NATS listens
  beyond loopback without TLS because --allow-plaintext-nats is set; steward
  tokens and job tokens cross the network in plain text. Set
  --nats-tls-cert-file and --nats-tls-key-file" nats=0.0.0.0:4222`.
- Step 3: exit code 2, "allow plaintext nats "maybe" is not true or false;
  pass --allow-plaintext-nats or set SCHMEIDE_ALLOW_PLAINTEXT_NATS".
- With `127.0.0.1`, `::1`, or `localhost`, or with both TLS files set, the
  control node starts without the flag and without a warning.

**Notes:**

- Covered by `internal/control` `TestCheckNATSTransport`,
  `TestRunRefusesPlaintextNATSBeyondLoopback`,
  `TestRunServesPlaintextNATSWhenAllowed`, `TestParseControlAllowPlaintextNATS`,
  and `TestListensBeyondLoopback`.

---

### JN-OPS-006: Restart the control node

**Preconditions:** A control node with registered runners, a running job, and
finished jobs. Its data directory is on persistent storage.

**Steps:**

1. Run `docker restart schmeide` (or stop and start `schmeide control`).
2. Run `schmeide status`.
3. From the App's Advanced tab, redeliver the label delivery that created a
   job which is still running or ended with a pull request.

**Expected result:**

- Step 2 lists the same runners and unfinished jobs as before.
- Runners reconnect on their own. A job a runner held continues; if its
  heartbeat lapsed, it is redelivered and resumes from its stage record
  (JN-FAIL-010).
- Step 3 answers `duplicate`, not `created`: the per-issue records survived.
- Jobs whose stewards had a registered runner before the restart still go to
  that steward, not the pool.

**Notes:**

- On SIGTERM or SIGINT the control node stops taking webhooks, waits up to 10
  seconds for requests in flight, then stops NATS. Give the container at
  least that long (`--stop-timeout 30`).
- Deliveries GitHub sent while the control node was down failed. Redeliver
  them from the App's Advanced tab.
- Covered by `TestTrackerRemembersRunnersAndStatusAcrossRestart`,
  `TestTrackerRestartDropsMalformed`, `TestRunLogsPlainHTTPAndShutsDown`.

---

### JN-OPS-007: Lose the data directory

**Preconditions:** A control node whose data directory was deleted or not on
persistent storage.

**Steps:**

1. Start the control node with an empty data directory.
2. Run `schmeide status`.

**Expected result:**

- Status shows no runners and no jobs until runners restart and register.
- Queued jobs are gone, and a runner cannot resume a job in progress.
- An issue that already had a job can get a second one on its next qualifying
  event. To queue a lost job, remove and re-add the ready label.

**Notes:**

- Issues and pull requests on GitHub are not affected.
- Restore a backup by putting the directory back before the control node
  starts ([../DEPLOYMENT.md](../DEPLOYMENT.md#persistent-storage)).
- No automated test.

---

### JN-OPS-008: A runner on a different version

**Preconditions:** The control node runs `v0.0.2`. A runner built at `v0.0.1`.

**Steps:**

1. Start the runner.

**Expected result:**

- The runner is accepted and takes jobs.
- The control node logs `level=WARN msg="runner version differs from the
  control node; upgrade one so they match" steward=<login> host=<host>
  runner_version=v0.0.1 control_version=v0.0.2`.
- No warning when either side is `dev` or a bare commit hash, or when both
  match.

**Notes:**

- `schmeide status --json` shows each runner's `version`.
- Covered by `TestRegistrationVersionSkew` and `TestSkewed`.

---

### JN-OPS-009: Read the logs

**Preconditions:** A running control node and runner.

**Steps:**

1. Run `docker logs -f schmeide` (or read the control node's stderr).
2. Read the runner's stderr.

**Expected result:**

- Both write structured `key=value` lines (Go `slog` text) to stderr.
- The control node logs startup, each webhook (`msg="webhook handled"` with
  `outcome`, or `msg="webhook rejected"` with the error), each runner
  registration, each job state any runner reports (`msg="job state"
  job=schmeide.jobs.<steward>.<repo id>.<issue> state=... detail=...`), and
  token failures.
- The runner logs registration, each job state, gate failures with the output
  tail, hand-backs, and the harness sessions' own output.

**Notes:**

- schmeide does not write log files. Collect stderr with the container
  runtime or service manager.
- Webhook payloads and tokens are not logged.
- No automated test of the full log set.

---

### JN-OPS-010: The webhook endpoint behind the proxy

**Preconditions:** JN-OPS-002 with the TLS proxy in front of port 8080.

**Steps:**

1. Open `https://<host>/webhook` in a browser.
2. In the App's settings, redeliver a recent delivery.

**Expected result:**

- Step 1 answers 405 "webhook deliveries must be POSTed".
- Step 2 answers 2xx with `ignored`, `duplicate`, `created`, or `replay`.

**Notes:**

- A slow client is cut off after 30 seconds for the whole request (10 for
  headers).
- Covered by `TestWebhookReadTimeoutCutsOffASlowClient`.
