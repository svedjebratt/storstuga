#!/usr/bin/env bash

set -euo pipefail

usage() {
	cat <<'EOF'
Usage: scripts/install-raspberry.sh

Builds storstuga locally and installs the binary plus systemd service on this
machine.

Optional environment variables:
  SHELLY_OFF_URL   Required to write a usable env file.
  SHELLY_ON_URL    Required to write a usable env file.
  WEBHOOK_TOKEN    Optional.
EOF
}

if [[ ${1:-} == "-h" || ${1:-} == "--help" || $# -gt 0 ]]; then
	usage
	exit 0
fi

root_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
build_dir=$(mktemp -d "${TMPDIR:-/tmp}/storstuga-build.XXXXXX")

cleanup() {
	rm -rf "$build_dir"
}
trap cleanup EXIT

binary_path="$build_dir/storstuga"
config_ready=true

if [[ ${SHELLY_OFF_URL:-} == "" || ${SHELLY_ON_URL:-} == "" ]]; then
	config_ready=false
fi

echo "building locally"
( cd "$root_dir" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$binary_path" . )

cat > "$build_dir/storstuga.service" <<'EOF'
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

cat > "$build_dir/storstuga.env" <<EOF
BIND_ADDR=:8080
DB_PATH=/var/lib/storstuga/data.db
CHECK_INTERVAL=30s
CHECK_TIMEOUT=4s
CHECK_TARGETS=1.1.1.1:53,8.8.8.8:53
FAILURE_THRESHOLD=3
RESTART_COOLDOWN=30m
RESTART_DELAY=5s
SHELLY_OFF_URL=${SHELLY_OFF_URL:-replace-me}
SHELLY_ON_URL=${SHELLY_ON_URL:-replace-me}
WEBHOOK_TOKEN=${WEBHOOK_TOKEN:-}
RETENTION_DAYS=90
RETENTION_INTERVAL=24h
EOF

sudo useradd --system --no-create-home --shell /usr/sbin/nologin storstuga >/dev/null 2>&1 || true
sudo install -d -o root -g root -m 0755 /opt/storstuga
sudo install -d -o storstuga -g storstuga -m 0755 /var/lib/storstuga
sudo install -d -o root -g root -m 0700 /etc/storstuga
sudo install -m 0755 "$binary_path" /opt/storstuga/storstuga
sudo install -m 0644 "$build_dir/storstuga.service" /etc/systemd/system/storstuga.service
sudo install -m 0600 "$build_dir/storstuga.env" /etc/storstuga/storstuga.env
sudo systemctl daemon-reload

if $config_ready; then
	sudo systemctl enable --now storstuga
	echo "installed and started on this machine"
else
	echo "installed on this machine"
	echo "fill in /etc/storstuga/storstuga.env, then run: sudo systemctl enable --now storstuga"
fi
