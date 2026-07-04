#!/usr/bin/env bash
#
# conduit-connector installer (Linux). Downloads the binary, installs it as a
# systemd service, and starts it. Outbound-only — opens no inbound port.
#
# Configuration comes from the environment (so an RMM can set site variables
# and run this unattended). Required:
#
#   RELAY_URL         wss:// relay endpoint (e.g. wss://conduit-wss.wyre.ai)
#   ENROLLMENT_TOKEN  the identity-only token from Conduit (site -> Deploy connector)
#
# Optional:
#   CONNECTOR_URL     direct URL to the connector binary (e.g. a signed link the
#                     Conduit wizard hands you). If unset, falls back to the
#                     GitHub Release named by CONNECTOR_VERSION.
#   CONNECTOR_VERSION release tag to pull from GitHub Releases (default: latest).
#                     For a private repo, also set GH_TOKEN.
#   LOG_LEVEL         debug|info|warn|error (default info)
#
# Usage (unattended):
#   RELAY_URL=wss://conduit-wss.wyre.ai ENROLLMENT_TOKEN=<jwt> \
#     CONNECTOR_URL=<signed-url> bash install.sh
#
set -euo pipefail

REPO="wyre-technology/conduit-connector"
BIN_PATH="/usr/local/bin/conduit-connector"
ENV_DIR="/etc/conduit-connector"
UNIT_PATH="/etc/systemd/system/conduit-connector.service"
LOG_LEVEL="${LOG_LEVEL:-info}"

die() { echo "install: $*" >&2; exit 1; }
need_root() { [ "$(id -u)" -eq 0 ] || SUDO="sudo"; }
SUDO=""
need_root

# --- preconditions ---
[ -n "${RELAY_URL:-}" ] || die "RELAY_URL is required (a wss:// relay endpoint)."
[ -n "${ENROLLMENT_TOKEN:-}" ] || die "ENROLLMENT_TOKEN is required (mint it in Conduit: site -> Deploy connector)."
case "$RELAY_URL" in
  wss://*) : ;;
  *) die "RELAY_URL must be wss:// (got: $RELAY_URL)";;
esac
command -v curl >/dev/null 2>&1 || die "curl is required."

# --- resolve architecture ---
arch="$(uname -m)"
case "$arch" in
  x86_64|amd64) asset="conduit-connector-linux-amd64" ;;
  aarch64|arm64) asset="conduit-connector-linux-arm64" ;;
  *) die "unsupported architecture: $arch (supported: x86_64, aarch64)";;
esac

# --- resolve download URL + fetch (once) ---
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
auth=()
if [ -n "${CONNECTOR_URL:-}" ]; then
  url="$CONNECTOR_URL"
  echo "install: downloading connector binary"
else
  ver="${CONNECTOR_VERSION:-latest}"
  [ -n "${GH_TOKEN:-}" ] && auth=(-H "Authorization: Bearer ${GH_TOKEN}")
  if [ "$ver" = "latest" ]; then
    url="https://github.com/${REPO}/releases/latest/download/${asset}"
  else
    url="https://github.com/${REPO}/releases/download/${ver}/${asset}"
  fi
  echo "install: downloading ${asset} (${ver}) from GitHub Releases"
fi
curl -fsSL "${auth[@]}" "$url" -o "$tmp/conduit-connector" \
  || die "download failed ($url). For a private repo set GH_TOKEN, or pass CONNECTOR_URL."
[ -s "$tmp/conduit-connector" ] || die "downloaded binary is empty"

# --- install binary ---
$SUDO systemctl stop conduit-connector 2>/dev/null || true
$SUDO install -m 0755 "$tmp/conduit-connector" "$BIN_PATH"

# --- write env (mode 600 — carries the enrollment token) ---
$SUDO install -d -m 0700 "$ENV_DIR"
umask 077
$SUDO tee "$ENV_DIR/env" >/dev/null <<EOF
RELAY_URL=${RELAY_URL}
ENROLLMENT_TOKEN=${ENROLLMENT_TOKEN}
LOG_LEVEL=${LOG_LEVEL}
EOF
$SUDO chmod 600 "$ENV_DIR/env"

# --- systemd unit (hardened; outbound-only) ---
$SUDO tee "$UNIT_PATH" >/dev/null <<'EOF'
[Unit]
Description=Conduit on-prem connector (dials out only; no inbound ports)
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/conduit-connector/env
ExecStart=/usr/local/bin/conduit-connector
Restart=always
RestartSec=5
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes

[Install]
WantedBy=multi-user.target
EOF

$SUDO systemctl daemon-reload
$SUDO systemctl enable --now conduit-connector

echo "install: done. The connector is running and dialing ${RELAY_URL}."
echo "install: follow logs with:  ${SUDO} journalctl -u conduit-connector -f"
