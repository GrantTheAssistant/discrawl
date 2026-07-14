#!/usr/bin/env bash
set -euo pipefail
umask 0077

[[ "${EUID}" -eq 0 ]] || { echo "restore-discrawl must run as root" >&2; exit 1; }
[[ "$#" -eq 2 ]] || { echo "usage: restore-discrawl BACKUP.db.gz BACKUP.db.gz.sha256" >&2; exit 2; }
archive="$1"
manifest="$2"
[[ -f "${archive}" && -f "${manifest}" ]] || { echo "backup and checksum manifest are required" >&2; exit 1; }
archive="$(readlink -f "${archive}")"
manifest="$(readlink -f "${manifest}")"
[[ "$(basename "${manifest}")" == "$(basename "${archive}").sha256" ]] || { echo "manifest name must match backup" >&2; exit 1; }
(cd "$(dirname "${archive}")" && sha256sum --check --status "$(basename "${manifest}")") || { echo "backup checksum mismatch" >&2; exit 1; }

guild_id="$(sed -n 's/^default_guild_id = "\([0-9][0-9]*\)"$/\1/p' /etc/discrawl/config.toml)"
token="$(sed -n 's/^DISCORD_BOT_TOKEN=//p' /etc/discrawl/bot-token.env)"
[[ "${guild_id}" =~ ^[0-9]{17,20}$ && "${token}" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "invalid installed guild or token" >&2; exit 1; }
memory_kib="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
(( memory_kib >= 3000000 )) || { echo "restore requires a temporary e2-medium (or larger); resize the stopped VM first" >&2; exit 1; }

staged="$(mktemp --tmpdir=/var/lib/discrawl/backups .restore.XXXXXX.db)"
uncompressed_bytes="$(gzip -cd -- "${archive}" | wc -c | tr -d ' ')"
wal_bytes=0
[[ -f /var/lib/discrawl/archive.db-wal ]] && wal_bytes="$(stat -c %s /var/lib/discrawl/archive.db-wal)"
available_bytes="$(df --output=avail -B1 /var/lib/discrawl | tail -1 | tr -d ' ')"
safety_bytes=$((uncompressed_bytes / 5))
(( safety_bytes >= 536870912 )) || safety_bytes=536870912
required_bytes=$((uncompressed_bytes + wal_bytes + safety_bytes))
(( available_bytes >= required_bytes )) || {
  rm -f "${staged}"
  echo "insufficient restore headroom: need ${required_bytes} free bytes, have ${available_bytes}; resize the PD" >&2
  exit 1
}
gzip -cd -- "${archive}" > "${staged}"
[[ "$(sqlite3 "${staged}" 'pragma quick_check;')" == ok ]] || { rm -f "${staged}"; echo "restored DB quick_check failed" >&2; exit 1; }
[[ "$(sqlite3 "${staged}" "select count(*) || ':' || coalesce(min(id),'') from guilds;")" == "1:${guild_id}" ]] || {
  rm -f "${staged}"; echo "restored DB has wrong guild scope" >&2; exit 1;
}

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
rollback_dir="/var/lib/discrawl/backups/pre-restore-${stamp}"
mkdir -m 0700 "${rollback_dir}"
new_db_installed=false

restore_old() {
  local rc=$?
  trap - ERR
  set +e
  rm -f /run/discrawl-sync.env
  systemctl stop discrawl-api.service discrawl-tail.service >/dev/null 2>&1
  for name in archive.db archive.db-wal archive.db-shm; do
    if [[ -e "${rollback_dir}/${name}" ]]; then
      rm -f "/var/lib/discrawl/${name}"
      mv -- "${rollback_dir}/${name}" "/var/lib/discrawl/${name}"
    elif [[ "${new_db_installed}" == true ]]; then
      rm -f "/var/lib/discrawl/${name}"
    fi
  done
  rm -f /var/lib/discrawl/projection/state.json
  systemctl start discrawl-tail.service >/dev/null 2>&1
  systemctl start discrawl-api.service >/dev/null 2>&1
  echo "restore failed; previous DB restored from ${rollback_dir}" >&2
  exit "${rc}"
}
trap restore_old ERR

systemctl stop discrawl-api.service discrawl-tail.service
restore_started="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
for path in /var/lib/discrawl/archive.db /var/lib/discrawl/archive.db-wal /var/lib/discrawl/archive.db-shm; do
  [[ -e "${path}" ]] && mv -- "${path}" "${rollback_dir}/"
done
chown discrawl-tail:discrawl "${staged}"
chmod 0640 "${staged}"
mv -- "${staged}" /var/lib/discrawl/archive.db
new_db_installed=true
# Keep the API offline while Discord repairs every stored channel. The durable
# Firestore tombstone ledger is outside SQLite and therefore survives restore.
unset token
printf 'DISCORD_GUILD_ID=%s\nDISCRAWL_SYNC_ARGS=--full\n' "${guild_id}" > /run/discrawl-sync.env
chown root:discrawl /run/discrawl-sync.env
chmod 0640 /run/discrawl-sync.env
systemctl start discrawl-sync.service
rm -f /run/discrawl-sync.env
[[ "$(sqlite3 /var/lib/discrawl/archive.db 'pragma quick_check;')" == ok ]]
[[ "$(sqlite3 /var/lib/discrawl/archive.db "select count(*) || ':' || coalesce(min(id),'') from guilds;")" == "1:${guild_id}" ]]
rm -f /var/lib/discrawl/projection/state.json
chown discrawl-tail:discrawl /var/lib/discrawl/archive.db
chmod 0640 /var/lib/discrawl/archive.db
find /var/lib/discrawl -maxdepth 1 -type f \( -name 'archive.db-wal' -o -name 'archive.db-shm' \) -exec chown discrawl-tail:discrawl {} + -exec chmod 0640 {} + 2>/dev/null || true
systemctl start discrawl-tail.service
systemctl start discrawl-api.service
systemctl is-active --quiet discrawl-tail.service
systemctl is-active --quiet discrawl-api.service

project_id="$(python3 -c 'import json; print(json.load(open("/etc/discrawl/archive-api.json"))["projection"]["project_id"])')"
org_id="$(python3 -c 'import json; print(json.load(open("/etc/discrawl/archive-api.json"))["projection"]["org_id"])')"
projection_url="https://firestore.googleapis.com/v1/projects/${project_id}/databases/(default)/documents/orgs/${org_id}/chatRuntime/discrawlProjection"
projection_healthy=false
for _ in $(seq 1 90); do
  access_token="$(curl --fail --silent --show-error --max-time 5 -H 'Metadata-Flavor: Google' \
    'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token' | \
    python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"
  projection_json="$(curl --fail --silent --show-error --max-time 15 -H "Authorization: Bearer ${access_token}" "${projection_url}" 2>/dev/null || true)"
  unset access_token
  if PROJECTION_JSON="${projection_json}" python3 - "${guild_id}" "${restore_started}" <<'PY'
import json, os, sys
from datetime import datetime, timezone, timedelta
try:
    f=json.loads(os.environ["PROJECTION_JSON"])["fields"]
    stamp=datetime.fromisoformat(f["lastSuccessAt"]["timestampValue"].replace("Z", "+00:00"))
    started=datetime.fromisoformat(sys.argv[2].replace("Z", "+00:00"))
    age=datetime.now(timezone.utc)-stamp
    ok=(f["state"]["stringValue"] == "healthy" and f["schemaVersion"]["integerValue"] == "2"
        and f["guildId"]["stringValue"] == sys.argv[1]
        and f["tombstoneSweepComplete"]["booleanValue"] is True
        and f["attachmentUrlSweepComplete"]["booleanValue"] is True
        and stamp >= started and -timedelta(minutes=20) <= age <= timedelta(minutes=20))
except (KeyError, TypeError, ValueError, json.JSONDecodeError):
    ok=False
raise SystemExit(0 if ok else 1)
PY
  then
    projection_healthy=true
    break
  fi
  sleep 10
done
[[ "${projection_healthy}" == true ]] || { echo "projection did not complete restore seeding and privacy sweeps" >&2; false; }
trap - ERR
echo "restore complete; rollback DB retained in ${rollback_dir}"
