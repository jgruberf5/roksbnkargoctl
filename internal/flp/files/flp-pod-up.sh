#!/usr/bin/env bash
# Brings up the f5-license-proxy stack as a podman pod (ported from roksbnkctl's
# flp_vsi module). Uses the roksbnkargoctl-injected
# CA (/opt/flp/ca.crt + ca.key) to sign the mTLS leaves; the images self-initialize
# (vault-init unseals Vault + issues the proxy's vault client cert; postgres initdb).
set -uo pipefail
set -a; . /opt/flp/answer.env; set +a
: "${JWT_TOKEN:?set JWT_TOKEN in answer.env}"
HOST="$(hostname)"; PRIV_IP="$(hostname -I | awk '{print $1}')"
B=/opt/flp; G=$B/gen; VOL=$B/vol_local; LLM=$B/llm; VCL=$B/vault_ssl_llm
VSSL=$B/vault/ssl; VDATA=$B/vault/data; VCONF=$B/vault/config; PGSSL=$B/pg/ssl; PGDATA=$B/pg/data

podman pod rm -f flp >/dev/null 2>&1 || true
rm -rf "$G" "$VOL" "$LLM" "$VCL" "$VSSL" "$VDATA" "$VCONF" "$PGSSL" "$PGDATA"
mkdir -p "$G" "$VOL/certs" "$VOL/db" "$LLM" "$VCL" "$VSSL" "$VDATA" "$VCONF" "$PGSSL" "$PGDATA"
cp /opt/flp/ca.crt "$G/ca.crt"; cp /opt/flp/ca.key "$G/ca.key"

echo "== FAR login =="
cat /opt/flp/cne_pull_64.json | podman login "${FAR_HOST:-repo.f5.com}" -u _json_key_base64 --password-stdin

echo "== leaves (signed by the injected CA) =="
leaf(){ n=$1 cn=$2 san=$3 kt=$4
  if [ "$kt" = ec ]; then openssl ecparam -name prime256v1 -genkey -out "$G/$n.key" 2>/dev/null
  else openssl genrsa -out "$G/$n.key" 4096 2>/dev/null; fi
  openssl req -new -key "$G/$n.key" -subj "/C=US/ST=WA/L=Seattle/O=F5/CN=$cn" -out "$G/$n.csr" 2>/dev/null
  openssl x509 -req -in "$G/$n.csr" -CA "$G/ca.crt" -CAkey "$G/ca.key" -CAcreateserial -days 3650 \
    -extfile <(printf "subjectAltName=%s" "$san") -out "$G/$n.crt" 2>/dev/null; }
FSAN="DNS:flp,DNS:localhost,DNS:$HOST,IP:$PRIV_IP"; [ -n "${EXTERNAL_IP:-}" ] && [ "${EXTERNAL_IP}" != "${PRIV_IP}" ] && FSAN="$FSAN,IP:${EXTERNAL_IP}"
leaf pg-server postgresql "DNS:postgresql,DNS:localhost,DNS:$HOST" ec
leaf pg-client llm-client "DNS:llm-client,DNS:localhost" ec
leaf vault vault "DNS:vault,DNS:localhost,DNS:vault-postgresql-service,DNS:$HOST" rsa
leaf flp flp "$FSAN" rsa

echo "== stage =="
cp "$G/ca.crt" "$VOL/certs/ca.crt"; cp "$G/pg-client.crt" "$VOL/certs/client.crt"; cp "$G/pg-client.key" "$VOL/certs/client.key"
cp /opt/flp/prod_jwks.txt "$VOL/certs/prod_jwks.txt"
cp "$G/ca.crt" "$LLM/ca.crt"; cp "$G/flp.crt" "$LLM/tls.crt"; cp "$G/flp.key" "$LLM/tls.key"
cp "$G/ca.crt" "$VSSL/ca.crt"; cp "$G/ca.crt" "$VSSL/root-ca.crt"; cp "$G/vault.crt" "$VSSL/tls.crt"; cp "$G/vault.key" "$VSSL/tls.key"
cp "$G/ca.crt" "$PGSSL/ca.crt"; cp "$G/pg-server.crt" "$PGSSL/tls.crt"; cp "$G/pg-server.key" "$PGSSL/tls.key"
chmod -R a+rX "$B"; find "$B" -name '*.key' -exec chmod 0644 {} +; chmod -R 0777 "$VDATA" "$VCL"

cat > "$VCONF/vault.hcl" <<'HCL'
storage "file" { path = "/etc/vault/data" }
listener "tcp" { address = "[::]:8200" tls_cert_file = "/etc/vault/ssl/tls.crt" tls_key_file = "/etc/vault/ssl/tls.key" tls_disable = 0 }
default_lease_ttl = "720h"
max_lease_ttl = "8760h"
disable_mlock = true
HCL

PGUSER=llm; PGPASSWORD="$(openssl rand -hex 16)"
echo "== pod =="
# Publish :80 too when the flp-status web UI is enabled (FLP_STATUS_IMAGE set).
PUB80=""; [ -n "${FLP_STATUS_IMAGE:-}" ] && PUB80="--publish 80:80"
podman pod create --name flp --publish 8443:8443 $PUB80 >/dev/null
podman run -d --restart=always --pod flp --name postgresql \
  -e POSTGRES_DB=llm_db -e POSTGRES_USER="$PGUSER" -e POSTGRES_PASSWORD="$PGPASSWORD" -e PGUSER="$PGUSER" -e PGPASSWORD="$PGPASSWORD" \
  -v "$PGSSL":/etc/pg/ssl:Z -v "$PGDATA":/var/lib/postgresql/data:Z "$REG/postgresql:$TAG"
podman run -d --restart=always --pod flp --name vault --cap-add=IPC_LOCK,SETFCAP \
  -v "$VCONF":/etc/vault/config:Z -v "$VSSL":/etc/vault/ssl:Z -v "$VDATA":/etc/vault/data:Z \
  "$REG/vault:$VAULT_TAG" server -config=/etc/vault/config/vault.hcl
podman run -d --restart=always --pod flp --name vault-init \
  -e VAULT_ADDR=https://localhost:8200 -e VAULT_CACERT=/etc/vault/ssl/root-ca.crt \
  -e CERT_ROTATION_ENABLED=true -e CERT_CHECK_INTERVAL=3600 -e CERT_RENEW_THRESHOLD_PERCENT=20 -e ISSUE_CERT=true \
  -v "$VSSL":/etc/vault/ssl:Z -v "$VCL":/etc/vault/ssl/llm:Z -v "$VDATA":/etc/vault/data:Z \
  "$REG/vault-init:$TAG" sh -c "sh vault-init/unseal.sh & tail -f /dev/null"
podman run -d --restart=always --pod flp --name f5-license-proxy \
  -e VAULT_INITIAL_DELAY_SECS=10 -e DB_RETRY_ATTEMPTS=15 -e VAULT_RETRY_ATTEMPTS=100 \
  -e VAULT_CLIENT_CERT_DIR=/etc/vault/ssl/llm -e VAULT_CERT_WATCHER_ENABLED=true -e VAULT_CERT_WATCHER_INTERVAL_SEC=60 \
  -e F5_CERT_URL="$F5_CERT_URL" -e F5_ENTITLEMENT_URL="$F5_ENTITLEMENT_URL" -e F5_INITIAL_CONFIG_URL="$F5_INITIAL_CONFIG_URL" \
  -e MODE_OF_OPERATION="$MODE_OF_OPERATION" -e SIGN_VERIFICATION_CERT_PATH=/vol/local/certs/prod_jwks.txt -e VOLUME_PATH=/vol/local \
  -e DATABASE_VENDOR_NAME=postgres -e PGDATABASE=llm_db -e PGHOST=localhost -e PGPORT=5432 -e PGUSER="$PGUSER" -e PGPASSWORD="$PGPASSWORD" \
  -e PGSSLROOTCERT=/vol/local/certs/ca.crt -e PGSSLCERT=/vol/local/certs/client.crt -e PGSSLKEY=/vol/local/certs/client.key \
  -e VAULT_ADDR=https://localhost:8200 -e VAULT_HOST=localhost -e VAULT_PORT=8200 -e IS_TLS_ENABLED=true \
  -e TLS_CA_CERT=/llm/ca.crt -e TLS_SERVER_CERT=/llm/tls.crt -e TLS_SERVER_KEY=/llm/tls.key -e TLS_SERVER_PORT=8443 -e TLS_SERVER_HOST=0.0.0.0 \
  -e IS_PROXY_ENABLED="${IS_PROXY_ENABLED:-false}" -e PROXY_HOST="${PROXY_HOST:-}" -e PROXY_PORT="${PROXY_PORT:-}" -e PROXY_PROTOCOL="${PROXY_PROTOCOL:-http}" \
  -e JWT_TOKEN="$JWT_TOKEN" \
  -v "$VOL":/vol/local:Z -v "$LLM":/llm:Z -v "$VSSL":/etc/vault/ssl:Z -v "$VCL":/etc/vault/ssl/llm:Z \
  "$REG/f5-license-proxy:$TAG"
# ── Optional flp-status web UI (mobile status page + /api/status + logs) ──────
# Runs as a container IN the pod, reading the other containers' state over the
# podman socket (CONTAINER_HOST). Serves :80, no auth (private read-only status).
# The image is pulled from FLP_STATUS_IMAGE (mirror or public); Harbor trust for
# the mirror is set up by cloud-init (certs.d) when FLP_REGISTRY_HOST is given.
if [ -n "${FLP_STATUS_IMAGE:-}" ]; then
  systemctl enable --now podman.socket >/dev/null 2>&1 || true
  podman run -d --restart=always --pod flp --name flp-status \
    -v /run/podman/podman.sock:/run/podman/podman.sock \
    -v /opt/flp/ca.crt:/opt/flp/ca.crt:ro \
    -e CONTAINER_HOST=unix:///run/podman/podman.sock \
    -e FLP_BACKEND=podman -e PORT=80 \
    -e FLP_ENDPOINT="https://$PRIV_IP:8443" \
    "$FLP_STATUS_IMAGE" >/dev/null 2>&1 || echo "warning: flp-status container failed to start" >&2
fi
touch /opt/flp/.provisioned

# ── Reboot durability ────────────────────────────────────────────────────────
# Ubuntu's podman ships NO podman-restart.service, and podman is daemonless, so
# --restart=always does NOT survive a host reboot on its own — the pod (and its
# :8443 publish, which lives on the infra container) simply never comes back.
# Generate systemd units for the pod so systemd recreates it on every boot; the
# data volumes persist, so Vault/postgres state carries over. The flp-health
# timer (see cloud-init) is the catch-all that re-stages if this fast path can't
# recover (e.g. a sealed Vault after an unclean reboot).
if command -v podman >/dev/null 2>&1; then
  ( cd /etc/systemd/system && podman generate systemd --new --files --name flp >/dev/null 2>&1 ) || true
  systemctl daemon-reload 2>/dev/null || true
  systemctl enable pod-flp.service >/dev/null 2>&1 || true
fi
echo "== FLP pod up on :8443 =="
