#!/usr/bin/env bash
set -euo pipefail
umask 0077

[[ "${EUID}" -eq 0 ]] || { echo "install-host must run as root" >&2; exit 1; }
stage="$(cd "$(dirname "$0")" && pwd)"
for file in install.env discrawl-release.tar.gz discrawl-release.sha256 config.toml archive-api.json \
  bot-token.env backup.env discrawl-tail.service discrawl-sync.service discrawl-api.service backup-discrawl.service \
  backup-discrawl.timer backup-discrawl.sh restore-discrawl.sh; do
  [[ -f "${stage}/${file}" ]] || { echo "missing staged ${file}" >&2; exit 1; }
done

# Runtime uploads use the metadata server and GCS JSON API, so the host does not
# need Cloud SDK credentials or packages. apt never receives the Discord token.
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq ca-certificates curl gzip python3 sqlite3 systemd-resolved >/dev/null

# GCE commonly publishes its DNS resolver on the metadata IP. Discord-facing
# units deny that IP, so they must use the local systemd-resolved stub while the
# resolver daemon performs upstream DNS outside their cgroup policy.
resolver_backup="$(mktemp)"
cp -L --preserve=mode -- /etc/resolv.conf "${resolver_backup}"
systemctl enable --now systemd-resolved.service
resolver_link="$(mktemp /etc/.resolv.conf.discrawl.XXXXXX)"
rm -f "${resolver_link}"
ln -s /run/systemd/resolve/stub-resolv.conf "${resolver_link}"
mv -Tf "${resolver_link}" /etc/resolv.conf
metadata_dns="$(getent ahostsv4 metadata.google.internal 2>/dev/null | awk 'NR == 1 {print $1}' || true)"
if [[ "$(readlink -f /etc/resolv.conf)" != /run/systemd/resolve/stub-resolv.conf ]] ||
   ! getent ahosts discord.com >/dev/null || [[ "${metadata_dns}" != 169.254.169.254 ]] ||
   ! curl --noproxy '*' --fail --silent --show-error --max-time 3 -H 'Metadata-Flavor: Google' \
     http://169.254.169.254/computeMetadata/v1/instance/id >/dev/null; then
  resolver_restore="$(mktemp /etc/.resolv.conf.discrawl.XXXXXX)"
  cp --preserve=mode -- "${resolver_backup}" "${resolver_restore}"
  chmod 0644 "${resolver_restore}"
  mv -Tf "${resolver_restore}" /etc/resolv.conf
  rm -f "${resolver_backup}"
  runuser -u nobody -- getent ahosts discord.com >/dev/null || true
  echo "failed to establish local DNS stub; restored previous resolv.conf" >&2
  exit 1
fi
rm -f "${resolver_backup}"

getent group discrawl >/dev/null || groupadd --system discrawl
ensure_user() {
  local user="$1"
  id "${user}" >/dev/null 2>&1 || useradd --system --gid discrawl --home-dir /nonexistent --no-create-home --shell /usr/sbin/nologin "${user}"
}
ensure_user discrawl-tail
ensure_user discrawl-api
ensure_user discrawl-backup

device=/dev/disk/by-id/google-discrawl-data
[[ -b "${device}" ]] || { echo "data disk ${device} is missing" >&2; exit 1; }
if [[ -z "$(blkid -s TYPE -o value "${device}" || true)" ]]; then
  mkfs.ext4 -F -L discrawl-data "${device}" >/dev/null
fi
[[ "$(blkid -s TYPE -o value "${device}")" == ext4 ]] || { echo "data disk must be ext4" >&2; exit 1; }
uuid="$(blkid -s UUID -o value "${device}")"
mkdir -p /var/lib/discrawl
if ! grep -qE "^[[:space:]]*UUID=${uuid}[[:space:]]+/var/lib/discrawl[[:space:]]" /etc/fstab; then
  printf 'UUID=%s /var/lib/discrawl ext4 defaults,nodev,nosuid 0 2\n' "${uuid}" >> /etc/fstab
fi
mountpoint -q /var/lib/discrawl || mount /var/lib/discrawl

if [[ ! -f /swapfile ]]; then
  fallocate -l 2G /swapfile
  chmod 0600 /swapfile
  mkswap /swapfile >/dev/null
fi
grep -qE '^/swapfile[[:space:]]+none[[:space:]]+swap[[:space:]]' /etc/fstab || printf '/swapfile none swap sw 0 0\n' >> /etc/fstab
swapon --show=NAME --noheadings | grep -qx /swapfile || swapon /swapfile

install -d -o discrawl-tail -g discrawl -m 0750 /var/lib/discrawl
install -d -o discrawl-tail -g discrawl -m 0750 /var/lib/discrawl/cache /var/lib/discrawl/logs /var/lib/discrawl/share
install -d -o discrawl-api -g discrawl -m 0750 /var/lib/discrawl/projection
install -d -o discrawl-backup -g discrawl -m 0750 /var/lib/discrawl/backups
install -d -o root -g discrawl -m 0750 /var/lib/discrawl/releases
install -d -o root -g discrawl -m 0750 /etc/discrawl

(cd "${stage}" && sha256sum --check --status discrawl-release.sha256) || { echo "release checksum mismatch on host" >&2; exit 1; }
release_dir="$(mktemp -d)"
rollback_armed=false
prior=""
cleanup() { rm -rf "${release_dir}"; rm -f /run/discrawl-sync.env; }
trap cleanup EXIT
tar -xzf "${stage}/discrawl-release.tar.gz" -C "${release_dir}"
discrawl_binary="$(find "${release_dir}" -type f -name discrawl -perm /0111 -print -quit)"
api_binary="$(find "${release_dir}" -type f -name discrawl-api -perm /0111 -print -quit)"
[[ -n "${discrawl_binary}" && -n "${api_binary}" ]] || { echo "release must contain executable discrawl and discrawl-api" >&2; exit 1; }
if [[ -x /usr/local/bin/discrawl && -x /usr/local/bin/discrawl-api ]]; then
  find /var/lib/discrawl/releases -mindepth 1 -maxdepth 1 -type d -exec rm -rf -- {} +
  prior="/var/lib/discrawl/releases/$(date -u +%Y%m%dT%H%M%SZ)"
  install -d -o root -g discrawl -m 0750 "${prior}"
  install -o root -g discrawl -m 0750 /usr/local/bin/discrawl "${prior}/discrawl"
  install -o root -g discrawl -m 0750 /usr/local/bin/discrawl-api "${prior}/discrawl-api"
  [[ -f /etc/discrawl/config.toml ]] && install -o root -g discrawl -m 0640 /etc/discrawl/config.toml "${prior}/config.toml"
  [[ -f /etc/discrawl/archive-api.json ]] && install -o root -g discrawl -m 0640 /etc/discrawl/archive-api.json "${prior}/archive-api.json"
  [[ -f /etc/discrawl/bot-token.env ]] && install -o root -g root -m 0400 /etc/discrawl/bot-token.env "${prior}/bot-token.env"
  [[ -f /etc/discrawl/backup.env ]] && install -o root -g root -m 0400 /etc/discrawl/backup.env "${prior}/backup.env"
  for asset in discrawl-tail.service discrawl-sync.service discrawl-api.service backup-discrawl.service backup-discrawl.timer; do
    [[ -f "/etc/systemd/system/${asset}" ]] && install -o root -g root -m 0644 "/etc/systemd/system/${asset}" "${prior}/${asset}"
  done
  for asset in backup-discrawl restore-discrawl; do
    [[ -f "/usr/local/sbin/${asset}" ]] && install -o root -g root -m 0755 "/usr/local/sbin/${asset}" "${prior}/${asset}"
  done
  if [[ -s /var/lib/discrawl/archive.db ]]; then
    live_bytes="$(stat -c %s /var/lib/discrawl/archive.db)"
    wal_bytes=0; [[ -f /var/lib/discrawl/archive.db-wal ]] && wal_bytes="$(stat -c %s /var/lib/discrawl/archive.db-wal)"
    available_bytes="$(df --output=avail -B1 /var/lib/discrawl | tail -1 | tr -d ' ')"
    safety_bytes=$((live_bytes / 5)); (( safety_bytes >= 536870912 )) || safety_bytes=536870912
    (( available_bytes >= 2 * live_bytes + wal_bytes + safety_bytes )) || { echo "insufficient pre-upgrade rollback/compression headroom" >&2; exit 1; }
    sqlite3 /var/lib/discrawl/archive.db ".timeout 5000" ".backup '${prior}/archive.db'"
    [[ "$(sqlite3 "${prior}/archive.db" 'pragma quick_check;')" == ok ]] || { echo "pre-upgrade DB backup failed integrity check" >&2; exit 1; }
    chmod 0640 "${prior}/archive.db"
    chown root:discrawl "${prior}/archive.db"
  fi
  checksum_files=(discrawl discrawl-api)
  [[ -f "${prior}/config.toml" ]] && checksum_files+=(config.toml)
  [[ -f "${prior}/archive-api.json" ]] && checksum_files+=(archive-api.json)
  [[ -f "${prior}/archive.db" ]] && checksum_files+=(archive.db)
  (cd "${prior}" && sha256sum "${checksum_files[@]}" > checksums.sha256)
fi

rollback_upgrade() {
  local rc="${1:-$?}"
  trap - ERR
  set +e
  local rollback_failed=false
  if [[ "${rollback_armed}" == true && -n "${prior}" ]]; then
    systemctl stop discrawl-api.service discrawl-tail.service >/dev/null 2>&1 || rollback_failed=true
    install -o root -g root -m 0755 "${prior}/discrawl" /usr/local/bin/discrawl || rollback_failed=true
    install -o root -g root -m 0755 "${prior}/discrawl-api" /usr/local/bin/discrawl-api || rollback_failed=true
    if [[ -f "${prior}/config.toml" ]]; then install -o root -g discrawl -m 0640 "${prior}/config.toml" /etc/discrawl/config.toml || rollback_failed=true; fi
    if [[ -f "${prior}/archive-api.json" ]]; then install -o root -g discrawl -m 0640 "${prior}/archive-api.json" /etc/discrawl/archive-api.json || rollback_failed=true; fi
    if [[ -f "${prior}/bot-token.env" ]]; then install -o discrawl-tail -g discrawl -m 0400 "${prior}/bot-token.env" /etc/discrawl/bot-token.env || rollback_failed=true; fi
    if [[ -f "${prior}/backup.env" ]]; then install -o discrawl-backup -g discrawl -m 0400 "${prior}/backup.env" /etc/discrawl/backup.env || rollback_failed=true; fi
    for asset in discrawl-tail.service discrawl-sync.service discrawl-api.service backup-discrawl.service backup-discrawl.timer; do
      if [[ -f "${prior}/${asset}" ]]; then install -o root -g root -m 0644 "${prior}/${asset}" "/etc/systemd/system/${asset}" || rollback_failed=true; fi
    done
    for asset in backup-discrawl restore-discrawl; do
      if [[ -f "${prior}/${asset}" ]]; then install -o root -g root -m 0755 "${prior}/${asset}" "/usr/local/sbin/${asset}" || rollback_failed=true; fi
    done
    if [[ -f "${prior}/archive.db" ]]; then
      rm -f /var/lib/discrawl/archive.db /var/lib/discrawl/archive.db-wal /var/lib/discrawl/archive.db-shm || rollback_failed=true
      cp "${prior}/archive.db" /var/lib/discrawl/archive.db || rollback_failed=true
      chown discrawl-tail:discrawl /var/lib/discrawl/archive.db || rollback_failed=true
      chmod 0640 /var/lib/discrawl/archive.db || rollback_failed=true
      rm -f /var/lib/discrawl/projection/state.json || rollback_failed=true
    fi
    systemctl daemon-reload >/dev/null 2>&1 || rollback_failed=true
    systemctl start discrawl-tail.service >/dev/null 2>&1 || rollback_failed=true
    systemctl start discrawl-api.service >/dev/null 2>&1 || rollback_failed=true
    systemctl is-active --quiet discrawl-tail.service || rollback_failed=true
    systemctl is-active --quiet discrawl-api.service || rollback_failed=true
    echo "upgrade failed; previous checked release and DB restored from ${prior}" >&2
    [[ "${rollback_failed}" == false ]] || echo "WARNING: one or more rollback steps failed; keep the VM isolated and recover from ${prior}" >&2
  fi
  exit "${rc}"
}
rollback_armed=true
trap rollback_upgrade ERR
install -o root -g root -m 0755 "${discrawl_binary}" /usr/local/bin/discrawl
install -o root -g root -m 0755 "${api_binary}" /usr/local/bin/discrawl-api
install -o root -g discrawl -m 0640 "${stage}/config.toml" /etc/discrawl/config.toml
install -o root -g discrawl -m 0640 "${stage}/archive-api.json" /etc/discrawl/archive-api.json
install -o discrawl-tail -g discrawl -m 0400 "${stage}/bot-token.env" /etc/discrawl/bot-token.env
rm -f "${stage}/bot-token.env"
install -o discrawl-backup -g discrawl -m 0400 "${stage}/backup.env" /etc/discrawl/backup.env
install -o root -g root -m 0755 "${stage}/backup-discrawl.sh" /usr/local/sbin/backup-discrawl
install -o root -g root -m 0755 "${stage}/restore-discrawl.sh" /usr/local/sbin/restore-discrawl
install -o root -g root -m 0644 "${stage}/discrawl-tail.service" /etc/systemd/system/discrawl-tail.service
install -o root -g root -m 0644 "${stage}/discrawl-sync.service" /etc/systemd/system/discrawl-sync.service
install -o root -g root -m 0644 "${stage}/discrawl-api.service" /etc/systemd/system/discrawl-api.service
install -o root -g root -m 0644 "${stage}/backup-discrawl.service" /etc/systemd/system/backup-discrawl.service
install -o root -g root -m 0644 "${stage}/backup-discrawl.timer" /etc/systemd/system/backup-discrawl.timer

guild_id="$(sed -n 's/^DISCORD_GUILD_ID=//p' "${stage}/install.env")"
token="$(sed -n 's/^DISCORD_BOT_TOKEN=//p' /etc/discrawl/bot-token.env)"
[[ "${guild_id}" =~ ^[0-9]{17,20}$ && "${token}" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "invalid staged guild or token" >&2; rollback_upgrade 1; }

systemctl daemon-reload
systemctl stop discrawl-api.service discrawl-tail.service
if [[ ! -s /var/lib/discrawl/archive.db ]]; then
  sync_args=--full
else
  # Opening the writer offline applies pending migrations before the read API
  # can observe the schema. The broad incremental pass also repairs deployment
  # downtime without repeating historical backfill.
  sync_args=
fi
unset token
printf 'DISCORD_GUILD_ID=%s\nDISCRAWL_SYNC_ARGS=%s\n' "${guild_id}" "${sync_args}" > /run/discrawl-sync.env
chown root:discrawl /run/discrawl-sync.env
chmod 0640 /run/discrawl-sync.env
systemctl start discrawl-sync.service
rm -f /run/discrawl-sync.env
[[ "$(sqlite3 /var/lib/discrawl/archive.db 'pragma quick_check;')" == ok ]] || { echo "archive quick_check failed" >&2; rollback_upgrade 1; }
[[ "$(sqlite3 /var/lib/discrawl/archive.db "select count(*) || ':' || coalesce(min(id),'') from guilds;")" == "1:${guild_id}" ]] || {
  echo "archive does not contain exactly configured guild" >&2; rollback_upgrade 1;
}
chown discrawl-tail:discrawl /var/lib/discrawl/archive.db
chmod 0640 /var/lib/discrawl/archive.db
find /var/lib/discrawl -maxdepth 1 -type f \( -name 'archive.db-wal' -o -name 'archive.db-shm' \) -exec chown discrawl-tail:discrawl {} + -exec chmod 0640 {} + 2>/dev/null || true

systemctl enable discrawl-tail.service discrawl-api.service
systemctl restart discrawl-tail.service
systemctl restart discrawl-api.service
systemctl enable --now backup-discrawl.timer
systemctl is-active --quiet discrawl-tail.service
systemctl is-active --quiet discrawl-api.service
rollback_armed=false
trap - ERR
if [[ -n "${prior}" && -f "${prior}/archive.db" ]]; then
  if gzip -9 "${prior}/archive.db"; then
    checksum_files=(discrawl discrawl-api)
    [[ -f "${prior}/config.toml" ]] && checksum_files+=(config.toml)
    [[ -f "${prior}/archive-api.json" ]] && checksum_files+=(archive-api.json)
    checksum_files+=(archive.db.gz)
    (cd "${prior}" && sha256sum "${checksum_files[@]}" > checksums.sha256)
  else
    echo "warning: prior rollback DB remains uncompressed" >&2
  fi
fi
