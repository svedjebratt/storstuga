# storstuga

Small self-hosted Go service that:

1. Monitors network availability.
2. Restarts power via Shelly plug if network is down repeatedly.
3. Accepts Shelly temperature webhooks and stores events in SQLite.

## Run

```bash
go run .
```

## Configuration

Environment variables:

- `BIND_ADDR` (default `:8080`)
- `DB_PATH` (default `./data.db`)
- `CHECK_INTERVAL` (default `30s`)
- `CHECK_TIMEOUT` (default `4s`)
- `CHECK_TARGETS` (default `1.1.1.1:53,8.8.8.8:53`)
- `FAILURE_THRESHOLD` (default `3`)
- `RESTART_COOLDOWN` (default `30m`)
- `RESTART_DELAY` (default `5s`)
- `SHELLY_OFF_URL` (required)
- `SHELLY_ON_URL` (required)
- `WEBHOOK_TOKEN` (optional)
- `RETENTION_DAYS` (default `90`)
- `RETENTION_INTERVAL` (default `24h`)

`CHECK_TARGETS` supports comma-separated TCP targets (`host:port`) and HTTP targets (`https://...`). If any target succeeds, the network is considered up.

## Endpoints

- `GET /healthz`
- `POST /webhook/shelly`

Webhook authorization:

- If `WEBHOOK_TOKEN` is set, pass token in `X-Webhook-Token` header or `?token=` query parameter.

## Example Shelly URLs

For Shelly Gen2 plug switch id `0` on host `192.168.1.50`:

- `SHELLY_OFF_URL=http://192.168.1.50/rpc/Switch.Set?id=0&on=false`
- `SHELLY_ON_URL=http://192.168.1.50/rpc/Switch.Set?id=0&on=true`

## Build binary

```bash
go build -o storstuga .
```

## Raspberry Pi install

Run this on the Raspberry Pi itself after cloning the repo and installing Go:

```bash
SHELLY_OFF_URL="http://192.168.1.50/rpc/Switch.Set?id=0&on=false" \
SHELLY_ON_URL="http://192.168.1.50/rpc/Switch.Set?id=0&on=true" \
scripts/install-raspberry.sh
```

If you omit the Shelly URLs, the script still installs the binary and service,
but leaves `/etc/storstuga/storstuga.env` for you to edit before enabling the service.

## Production layout

- Binary: `/opt/storstuga/storstuga`
- Env config: `/etc/storstuga/storstuga.env`
- SQLite DB: `/var/lib/storstuga/data.db`
- Logs: `journald` via `systemd`

Create directories and service user:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin storstuga
sudo mkdir -p /opt/storstuga /etc/storstuga /var/lib/storstuga
sudo chown -R storstuga:storstuga /opt/storstuga /var/lib/storstuga
sudo chmod 700 /etc/storstuga
```

Install binary:

```bash
sudo install -m 0755 ./storstuga /opt/storstuga/storstuga
```

Create `/etc/storstuga/storstuga.env`:

```dotenv
BIND_ADDR=:8080
DB_PATH=/var/lib/storstuga/data.db
CHECK_INTERVAL=30s
CHECK_TIMEOUT=4s
CHECK_TARGETS=1.1.1.1:53,8.8.8.8:53
FAILURE_THRESHOLD=3
RESTART_COOLDOWN=30m
RESTART_DELAY=5s
SHELLY_OFF_URL=http://192.168.1.50/rpc/Switch.Set?id=0&on=false
SHELLY_ON_URL=http://192.168.1.50/rpc/Switch.Set?id=0&on=true
WEBHOOK_TOKEN=replace-with-long-random-token
RETENTION_DAYS=90
RETENTION_INTERVAL=24h
```

Secure the env file:

```bash
sudo chown root:root /etc/storstuga/storstuga.env
sudo chmod 600 /etc/storstuga/storstuga.env
```

## Recommended systemd unit

```ini
[Unit]
Description=Storstuga network monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=storstuga
Group=storstuga
WorkingDirectory=/var/lib/storstuga
ExecStart=/opt/storstuga/storstuga
Restart=always
RestartSec=5
EnvironmentFile=/etc/storstuga/storstuga.env
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/storstuga

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
sudo tee /etc/systemd/system/storstuga.service >/dev/null <<'EOF'
[Unit]
Description=Storstuga network monitor
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=storstuga
Group=storstuga
WorkingDirectory=/var/lib/storstuga
ExecStart=/opt/storstuga/storstuga
Restart=always
RestartSec=5
EnvironmentFile=/etc/storstuga/storstuga.env
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/storstuga

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl enable --now storstuga
sudo systemctl status storstuga
```

View logs:

```bash
journalctl -u storstuga -f
```

## Dev vs production

- Local development: use `.env.local` or shell exports.
- Production: use `/etc/storstuga/storstuga.env` and do not commit secrets.
