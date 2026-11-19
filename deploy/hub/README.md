# Pulse hub

`pulse-hub` is Pulse without the menubar: the same engine, API, alerts and watches, on an always-on
Linux server. Jobs on cloud VMs report to it while your Mac sleeps. This guide deploys it on a free
GCP e2-micro behind a Cloudflare Tunnel protected by Cloudflare Access. Everything here fits in the
free tiers.

Read [the security model](../../docs/security.md) ("Hub behind a tunnel") first: this is the one
setup where Pulse is reachable from the internet.

```
job VM ── https + Access service token + job token ──▶ Cloudflare edge (Access)
                                                          │ tunnel (outbound from the hub)
                                                          ▼
                              hub VM: cloudflared ──▶ 127.0.0.1:7778  push routes only
                                                      127.0.0.1:7777  local API (SSH only)
```

## 1. The VM (GCP, free tier)

The free tier covers one e2-micro in us-west1, us-central1 or us-east1 with a 30 GB standard disk.
A public IPv4 address is **not** free (about $3.65/month); a public IPv6 address is, and cloudflared
works over IPv6, so the VM gets no public IPv4 at all.

```bash
P=<project>; R=us-central1
gcloud --project $P compute networks create pulse-net --subnet-mode=custom
gcloud --project $P compute networks subnets create pulse-$R --network=pulse-net --region=$R \
  --range=10.42.0.0/24 --stack-type=IPV4_IPV6 --ipv6-access-type=EXTERNAL
# The only inbound rule: SSH through Identity-Aware Proxy.
gcloud --project $P compute firewall-rules create pulse-allow-iap-ssh --network=pulse-net \
  --direction=INGRESS --action=ALLOW --rules=tcp:22 --source-ranges=35.235.240.0/20
gcloud --project $P compute instances create pulse-hub --zone=$R-a --machine-type=e2-micro \
  --subnet=pulse-$R --stack-type=IPV4_IPV6 --no-address --ipv6-network-tier=PREMIUM \
  --image-family=debian-12 --image-project=debian-cloud --boot-disk-type=pd-standard --boot-disk-size=30GB \
  --shielded-secure-boot --shielded-vtpm --shielded-integrity-monitoring \
  --no-service-account --no-scopes \
  --metadata=enable-oslogin=TRUE,block-project-ssh-keys=TRUE,serial-port-enable=FALSE \
  --labels=app=pulse-hub
gcloud --project $P compute disks add-labels pulse-hub --zone=$R-a --labels=app=pulse-hub
```

Add a budget alert filtered on the `app=pulse-hub` label (in the billing account's currency) so a
surprise charge gets noticed. SSH is always `gcloud compute ssh pulse-hub --tunnel-through-iap`.

On the VM: automatic security updates, cloudflared from Cloudflare's apt repository, and a user for
each service.

```bash
sudo apt-get install -y unattended-upgrades && sudo dpkg-reconfigure -f noninteractive unattended-upgrades
# cloudflared: add https://pkg.cloudflare.com (signed apt repo), then: sudo apt-get install cloudflared
sudo useradd --system --home-dir /var/lib/pulse --create-home --shell /usr/sbin/nologin pulse
sudo chmod 700 /var/lib/pulse
sudo useradd --system --no-create-home --shell /usr/sbin/nologin cloudflared
```

## 2. Pulse

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o pulse-hub ./cmd/pulse-hub
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o pulse ./cmd/pulse
gcloud compute scp --tunnel-through-iap pulse-hub pulse deploy/hub/pulse-hub.service deploy/hub/pulse-admin pulse-hub:/tmp/
```

On the VM:

```bash
sudo install -m 0755 /tmp/pulse-hub /tmp/pulse /tmp/pulse-admin /usr/local/bin/
sudo install -m 0644 /tmp/pulse-hub.service /etc/systemd/system/
sudo systemctl enable --now pulse-hub
pulse-admin remote enable --tunnel https://<HOSTNAME>   # push listener on 127.0.0.1:7778
```

`pulse-admin` runs the `pulse` CLI as the hub's user (`pulse-admin ls`, `pulse-admin show <job>`,
`pulse-admin remote token <job>`). `systemd-analyze security pulse-hub` should rate it about 1.5.

## 3. Cloudflare

1. **Access first**, so the hostname is never reachable without it. In Zero Trust, create a
   self-hosted Access application for `<HOSTNAME>` with one policy: *Service Auth*, including one
   service token **per job machine** (or per project). Add a token to the policy for each machine
   you connect, so a compromised machine is cut off without touching the others.
2. **Tunnel.** `cloudflared tunnel create pulse-hub`, then
   `cloudflared tunnel route dns pulse-hub <HOSTNAME>` (from a machine where
   `cloudflared tunnel login` was done for that zone). Copy the tunnel's credentials JSON to
   `/etc/cloudflared/pulse-hub.json` (owner `cloudflared`, mode 0400), fill in
   [cloudflared-config.yml](cloudflared-config.yml) as `/etc/cloudflared/pulse-hub.yml`, install
   [cloudflared-pulse.service](cloudflared-pulse.service) and run
   `sudo systemctl enable --now cloudflared-pulse`.

Check from anywhere: without the service token, `curl https://<HOSTNAME>/api/v1/health` gets 403
from Cloudflare; with it, `{"ok":true,...}`. Admin API and UI paths are 404 through the tunnel.

## 4. Web UI (optional, read-only)

To check on jobs from a phone or any browser, let people sign in through Access:

1. In the Access application, add a policy: *Allow*, include the emails that may look. Set the
   application's cookie **SameSite to Lax**: with *Strict*, the redirect back from the login page
   arrives without the cookie and the browser loops ("too many redirects").
2. On the hub, name the same people (Pulse checks Access's token itself, so a mistake in the Access
   policy alone doesn't let anyone in). The audience tag is on the application's overview page.

```bash
pulse-admin remote web --team <team>.cloudflareaccess.com --aud <audience tag> --operator you@example.com
```

`--email` lets someone look; `--operator` also lets them stop runs and answer questions (from the
app, or the buttons on a question's notification). Both can turn on notifications on their phone.

Open `https://<HOSTNAME>`, sign in, and Pulse shows its jobs, charts and output. Shell actions,
alerts and settings stay on the hub. Viewers see everything jobs report,
command lines and output included, so list only people you'd show your terminal.
`pulse-admin remote web off` turns it off; open pages stop updating within 30 seconds.

## 5. Connecting job machines

From your own computer, once:

```bash
pulse hub setup --url https://<HOSTNAME> \
  --admin "gcloud compute ssh pulse-hub --zone=<zone> --tunnel-through-iap --quiet --command" \
  --cloudflare <account id>/<access app id>/<service-auth policy id>
pbpaste | pulse hub setup --cloudflare-token-stdin   # an API token with Access: Service Tokens
                                                     # and Access: Apps and Policies, Edit
```

Then, per machine:

```bash
pulse hub connect train --name gpu-1                       # writes ./pulse-gpu-1.env (0600)
pulse hub connect train --name gpu-2 --gcp-secret pulse-gpu-2 --gcp-project <p> \
  --grant <vm service account>                             # or straight into Secret Manager
```

`connect` creates the machine's own job token on the hub and its own Access service token, waits
until the hub accepts them, and prints what to run on the machine (the install one-liner, loading
the secret, `pulse run`). Secrets never appear on screen or in command lines.
`pulse hub disconnect gpu-1` revokes both tokens; `pulse hub list` shows what's connected.

By hand instead: `pulse-admin remote token <job>` on the hub, a service token added to the Access
policy, and on the machine:

```bash
curl -fsSL https://raw.githubusercontent.com/shhossain/pulse/main/install.sh | sh
export PULSE_URL=https://<HOSTNAME> PULSE_TOKEN=<job token>
export PULSE_CF_ACCESS_CLIENT_ID=<id>.access PULSE_CF_ACCESS_CLIENT_SECRET=<secret>
pulse run -n <job> -- python train.py     # progress, metrics and sys.* (CPU, memory, GPU, disk)
```

## If a job machine is compromised

It can only report its own job, within the limits in [security.md](../../docs/security.md). Cut it off:

```bash
pulse hub disconnect <machine>            # from your computer: both tokens at once
# or on the hub: pulse-admin remote tokens, then pulse-admin remote revoke <id>
```

(By hand, also revoke its service token in Zero Trust, Access → Service auth.) Then check
`sudo grep -E 'limit.exceeded|token' /var/lib/pulse/audit.log`, and `pulse-admin rm <job>` if you
don't trust what it sent.

## Updating

Rebuild, copy, `sudo install` the binaries and `sudo systemctl restart pulse-hub`. Data lives in
`/var/lib/pulse`.
