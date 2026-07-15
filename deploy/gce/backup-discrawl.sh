#!/usr/bin/env bash
set -euo pipefail
umask 0077

: "${DISCRAWL_DB:=/var/lib/discrawl/archive.db}"
: "${DISCRAWL_BACKUP_BUCKET:?set the tenant-local backup bucket}"

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
day="$(date -u +%d)"
year_month="$(date -u +%Y/%m)"
work_dir="$(mktemp -d --tmpdir=/var/lib/discrawl/backups .archive-backup.XXXXXX)"
trap 'rm -rf "${work_dir}"' EXIT
db="${work_dir}/archive-${timestamp}.db"
compressed="${db}.gz"
manifest="${compressed}.sha256"

live_bytes="$(stat -c %s "${DISCRAWL_DB}")"
wal_bytes=0
[[ -f "${DISCRAWL_DB}-wal" ]] && wal_bytes="$(stat -c %s "${DISCRAWL_DB}-wal")"
available_bytes="$(df --output=avail -B1 /var/lib/discrawl | tail -1 | tr -d ' ')"
safety_bytes=$((live_bytes / 5))
(( safety_bytes >= 536870912 )) || safety_bytes=536870912
# sqlite .backup and the gzip output coexist until gzip completes. Assume the
# compressed stream could be as large as the source instead of betting the
# nightly job on a favorable compression ratio.
required_bytes=$((2 * live_bytes + wal_bytes + safety_bytes))
(( available_bytes >= required_bytes )) || {
  echo "insufficient backup headroom: need ${required_bytes} free bytes, have ${available_bytes}" >&2
  exit 1
}

sqlite3 "${DISCRAWL_DB}" ".timeout 5000" ".backup '${db}'"
[[ "$(sqlite3 "${db}" 'pragma quick_check;')" == ok ]] || { echo "backup quick_check failed" >&2; exit 1; }
gzip -9 "${db}"
(cd "${work_dir}" && sha256sum "$(basename "${compressed}")" > "$(basename "${manifest}")")

upload_pair() {
  local prefix="$1"
  upload_object "${compressed}" "${prefix}/$(basename "${compressed}")" application/gzip
  upload_object "${manifest}" "${prefix}/$(basename "${manifest}")" text/plain
}

upload_object() {
  local source="$1" object="$2" content_type="$3" access_token encoded
  access_token="$(curl --fail --silent --show-error --max-time 5 -H 'Metadata-Flavor: Google' \
    'http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token' | \
    python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"
  encoded="$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1], safe=""))' "${object}")"
  curl --fail --silent --show-error --max-time 600 -X POST \
    -H "Authorization: Bearer ${access_token}" -H "Content-Type: ${content_type}" \
    --data-binary "@${source}" \
    "https://storage.googleapis.com/upload/storage/v1/b/${DISCRAWL_BACKUP_BUCKET}/o?uploadType=media&ifGenerationMatch=0&name=${encoded}" \
    >/dev/null
  unset access_token
}
upload_pair "daily/${year_month}"
if [[ "${day}" == 01 ]]; then
  upload_pair "monthly/$(date -u +%Y)"
fi
