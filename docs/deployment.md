# Production deployment

This runbook deploys the tested QuorumLimiter image on a single Ubuntu host with
a real domain, automatic HTTPS, persistent volumes, a firewall, backups, and a
verification procedure. It also describes a three-virtual-machine variant.

> **Membership is fixed at three nodes.** QuorumLimiter does not support dynamic
> membership; you deploy exactly three nodes and cannot add or remove them at
> runtime. See [architecture.md](architecture.md).

Throughout, replace `limiter.example.com` with your domain and
`ghcr.io/you/quorumlimiter:1.0.0` with your image reference.

---

## 1. Prerequisites

- **Server:** Ubuntu 24.04 or 26.04 LTS, at least **2 vCPU, 4 GB RAM, 40 GB disk**
  for a small demo.
- **DNS:** an `A` (and/or `AAAA`) record for `limiter.example.com` pointing at the
  server's public IP. Certificate issuance fails until this resolves publicly.
- **A built, tagged image** reachable by the server — either pushed to a registry
  (so `docker compose pull` works) or `docker load`ed onto the host. Build and tag
  it from a clean checkout:

  ```bash
  docker build -t ghcr.io/you/quorumlimiter:1.0.0 .
  docker push ghcr.io/you/quorumlimiter:1.0.0     # or: docker save | ssh host docker load
  ```

## 2. Install Docker (official repository)

```bash
sudo apt-get update
sudo apt-get install -y ca-certificates curl
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] \
  https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
  | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null
sudo apt-get update
sudo apt-get install -y docker-ce docker-ce-cli containerd.io docker-compose-plugin
```

## 3. Lay out the deployment directory

Copy **only** the deployment files (not the whole repo) to the server:

```bash
sudo install -d -m 0750 -o "$USER" -g "$USER" /opt/quorumlimiter
# from your workstation:
scp deploy/compose.prod.yml deploy/Caddyfile deploy/prometheus.yml deploy/alerts.yml \
    you@server:/opt/quorumlimiter/deploy/
```

## 4. Generate production secrets (on the server)

**Never reuse development secrets.** Generate fresh values directly on the host:

```bash
cd /opt/quorumlimiter
umask 077
cat > deploy/.env <<EOF
# Production secrets — generated on the server, git-ignored, never committed.
QL_CLUSTER_TOKEN=$(openssl rand -base64 36 | tr -d '\n')
QL_ADMIN_TOKEN=$(openssl rand -base64 36 | tr -d '\n')
QL_SESSION_KEY=$(openssl rand -base64 48 | tr -d '\n')
QL_API_KEY_PEPPER=$(openssl rand -base64 48 | tr -d '\n')

# Host-specific
QL_DOMAIN=limiter.example.com
QL_IMAGE=ghcr.io/you/quorumlimiter:1.0.0
EOF
chmod 600 deploy/.env
```

`compose.prod.yml` reads every secret and host value as an environment reference
(`${QL_...:?}`); it contains no secret values and fails fast if any are missing.

## 5. Firewall

Allow only SSH, HTTP, and HTTPS. Port 80 is required for the ACME HTTP-01
challenge and the HTTP→HTTPS redirect; 443 serves the site.

```bash
sudo ufw allow OpenSSH
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw enable
sudo ufw status verbose
```

Node debug ports are not published at all, and Prometheus binds to `127.0.0.1`
only — reach it with an SSH tunnel (`ssh -L 9090:127.0.0.1:9090 you@server`),
never over the public internet.

## 6. First deployment

```bash
cd /opt/quorumlimiter
docker compose -f deploy/compose.prod.yml --env-file deploy/.env config   # validate
docker compose -f deploy/compose.prod.yml --env-file deploy/.env pull
docker compose -f deploy/compose.prod.yml --env-file deploy/.env up -d
docker compose -f deploy/compose.prod.yml --env-file deploy/.env ps
docker compose -f deploy/compose.prod.yml --env-file deploy/.env logs caddy --tail=100
```

Watch the Caddy logs for successful certificate issuance for your domain. The
three nodes should report `healthy`; exactly one becomes leader.

## 7. Verification checklist

From a **different computer** (not the server):

```bash
curl -i  https://limiter.example.com/health/live      # 200
curl -i  https://limiter.example.com/health/ready      # 200 once a leader exists
curl -I  http://limiter.example.com/admin/login        # 308 redirect to https
curl -I  https://limiter.example.com/admin/login       # 200, Set-Cookie ... Secure
curl -i  https://limiter.example.com/internal/raft/status  # 404 (blocked at gateway)
curl -i  https://limiter.example.com/metrics               # 404 (blocked at gateway)
```

Confirm each of these:

- [ ] **HTTPS + certificate chain** valid (`curl -v` shows a trusted chain; no `-k`).
- [ ] **HTTP → HTTPS redirect** works (port 80 returns a redirect).
- [ ] **Security headers** present: `Strict-Transport-Security`,
      `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`.
- [ ] **Admin login** works and the session cookie is `Secure`, `HttpOnly`,
      `SameSite=Strict`.
- [ ] **Client decision**: create a policy + client in the dashboard, then a
      decision through the domain returns `200 allowed`.
- [ ] **Denial**: exhaust a small bucket → `429` with `Retry-After`.
- [ ] **Internal/metrics** routes return `404` publicly (checked above).
- [ ] **Prometheus** scrapes all three nodes (via the SSH tunnel, `up == 1`).
- [ ] **Leader failover**: `docker compose ... stop node<leader>`; a new leader
      appears within ~1–2 s and decisions keep succeeding.
- [ ] **Node restart + persistence**: restart a node (or the whole host); it
      rejoins as a follower and previously created policies/clients survive.

## 8. Logs

```bash
docker compose -f deploy/compose.prod.yml --env-file deploy/.env logs -f --tail=100        # all
docker compose -f deploy/compose.prod.yml --env-file deploy/.env logs -f node1              # one node
docker compose -f deploy/compose.prod.yml --env-file deploy/.env logs caddy --since=1h      # gateway/TLS
```

Application logs are JSON and redacted (no secrets, tokens, cookies, or full
subjects — see [security.md](security.md)).

## 9. Backups

Each node holds an independent bbolt database in its named volume
(`quorumlimiter_node1_data`, `_node2_data`, `_node3_data`). Because all three
replicate the same log, a backup of **any one healthy node** is a complete state
snapshot; back up all three for redundancy. Also back up `caddy_data` (issued
certificates).

Take a consistent, **encrypted** snapshot of each volume:

```bash
mkdir -p /opt/quorumlimiter/backups && cd /opt/quorumlimiter/backups
for v in node1_data node2_data node3_data caddy_data; do
  docker run --rm -v quorumlimiter_$v:/src:ro -v "$PWD":/out alpine \
    tar czf /out/$v-$(date +%F).tar.gz -C /src .
done
# Encrypt with a key kept OFF the server (age or gpg):
age -r "$BACKUP_AGE_RECIPIENT" -o node1_data-$(date +%F).tar.gz.age node1_data-$(date +%F).tar.gz
# ... repeat, then remove the plaintext archives and copy the .age files off-host.
```

Schedule this daily (cron or a systemd timer) and store the encrypted archives
and the backup key in separate locations. Do **not** commit backups or keys.

## 10. Restore rehearsal (on a separate machine)

Rehearse restore regularly — an untested backup is not a backup.

```bash
# On a clean host with Docker and the deploy files:
gpg/age --decrypt node1_data-DATE.tar.gz.age > node1_data.tar.gz   # decrypt first
docker volume create quorumlimiter_node1_data
docker run --rm -v quorumlimiter_node1_data:/dst -v "$PWD":/in alpine \
  sh -c 'cd /dst && tar xzf /in/node1_data.tar.gz'
# Restore the other volumes the same way, provide a deploy/.env with the SAME
# secrets, then `up -d` and run the verification checklist. Confirm policies,
# clients, and decision history are intact.
```

Restoring with different `QL_SESSION_KEY`/`QL_API_KEY_PEPPER` invalidates existing
sessions and API keys — keep the production secrets with the backups.

## 11. Image upgrade

```bash
# Point QL_IMAGE at the new tag (edit deploy/.env), then:
docker compose -f deploy/compose.prod.yml --env-file deploy/.env pull
docker compose -f deploy/compose.prod.yml --env-file deploy/.env up -d
docker compose -f deploy/compose.prod.yml --env-file deploy/.env ps
```

Compose recreates containers whose image changed and reuses the named volumes, so
data persists. Nodes restart one recreation at a time; the cluster keeps a quorum
and re-elects if the leader is replaced. Take a backup (step 9) before upgrading.

## 12. Rollback (without deleting volumes)

If a new image misbehaves, roll back to the previous tag **without touching the
volumes**:

```bash
# Set QL_IMAGE back to the previous known-good tag in deploy/.env, then:
docker compose -f deploy/compose.prod.yml --env-file deploy/.env up -d
```

Never run `down -v` or `docker volume rm` during a rollback — that would erase the
databases. Only the image reference changes; the data volumes are untouched.

## 13. Single-host limitation

Three containers on one server is convenient for a demo but **does not survive
host failure**: if the machine dies, all three replicas die with it, and Raft
provides no protection against the loss of the whole quorum at once. It protects
against a single *process/container* failure (and validates the consensus
mechanics), not against infrastructure loss. For real resilience, use the
three-machine variant below.

---

## 14. Three-virtual-machine variant

Run one node per VM so a single machine failure leaves a two-node quorum.
Membership is still fixed at three.

1. Provision three VMs on a **private network** (e.g. `10.0.0.11/12/13`). Give
   each a data disk.
2. On each VM, run only its own node container (a per-node Compose file, or
   `docker run`) with that node's identity and the shared peers list using the
   **private** addresses:

   ```
   QL_NODE_ID=node1
   QL_ADVERTISE_URL=http://10.0.0.11:8080
   QL_PEERS=node1=http://10.0.0.11:8080,node2=http://10.0.0.12:8080,node3=http://10.0.0.13:8080
   ```

3. **Firewall:** allow the Raft/HTTP port (8080) **only between the three private
   addresses** — never from the public internet:

   ```bash
   sudo ufw allow from 10.0.0.11 to any port 8080 proto tcp
   sudo ufw allow from 10.0.0.12 to any port 8080 proto tcp
   sudo ufw allow from 10.0.0.13 to any port 8080 proto tcp
   ```

4. Run Caddy on one VM (or a separate gateway/load balancer) that reverse-proxies
   to the three private node addresses, and publish only 80/443 from that host.
5. Keep `/internal/*` and `/metrics` unreachable from the public gateway (the
   provided Caddyfile already returns 404 for them).

This tolerates the loss of any one VM. It still does not add or remove nodes at
runtime — membership remains fixed at three.

---

## Manual actions you must perform

These steps are outside the repository and are your responsibility:

- Provision the server(s) and (for the 3-VM variant) the private network.
- Create the DNS record(s) and wait for propagation before first deploy.
- Build, tag, and publish (or load) the image; set `QL_IMAGE`.
- Generate and safeguard production secrets in `deploy/.env` on the server.
- Configure the firewall (`ufw`).
- Schedule backups and periodically rehearse a restore.
- Store backups and the backup key off-host, separately from each other.
