#!/usr/bin/env bash
set -euo pipefail
umask 0077
verify_started="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"

required=(PROJECT_ID REGION ZONE VM_NAME VPC_NETWORK VM_SUBNET VM_SUBNET_RANGE SERVERLESS_SUBNET \
  SERVERLESS_SUBNET_RANGE ARCHIVE_INTERNAL_IP ARCHIVE_SERVICE_ACCOUNT BACKUP_BUCKET \
  CLOUD_RUN_CALLER_SERVICE_ACCOUNT DISCORD_GUILD_ID ORG_ID ARCHIVE_AUDIENCE BOT_SECRET_ID)
for name in "${required[@]}"; do
  [[ -n "${!name:-}" ]] || { echo "missing ${name}" >&2; exit 1; }
done
archive_sa="${ARCHIVE_SERVICE_ACCOUNT}@${PROJECT_ID}.iam.gserviceaccount.com"
router="${VM_NAME}-router"
nat="${VM_NAME}-public-nat"

instance_json="$(gcloud compute instances describe "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --format=json)"
INSTANCE_JSON="${instance_json}" python3 - "${VPC_NETWORK}" "${VM_SUBNET}" "${ARCHIVE_INTERNAL_IP}" "${archive_sa}" "${VM_NAME}-data" <<'PY'
import json, os, sys
d=json.loads(os.environ["INSTANCE_JSON"]); network, subnet, ip, sa, disk=sys.argv[1:]
assert len(d["networkInterfaces"]) == 1
assert not d.get("tags", {}).get("items")
nic=d["networkInterfaces"][0]
assert nic["network"].endswith("/"+network) and nic["subnetwork"].endswith("/"+subnet)
assert nic["networkIP"] == ip and not nic.get("accessConfigs")
assert d.get("deletionProtection") is True
assert d["serviceAccounts"][0]["email"] == sa
assert "https://www.googleapis.com/auth/cloud-platform" in d["serviceAccounts"][0].get("scopes", [])
assert d["machineType"].endswith("/e2-micro")
data=[x for x in d.get("disks", []) if x.get("deviceName") == "discrawl-data"]
assert len(data) == 1 and data[0].get("autoDelete") is False and data[0].get("mode") == "READ_WRITE"
assert data[0].get("source", "").endswith("/disks/"+disk)
PY

[[ "$(gcloud compute networks describe "${VPC_NETWORK}" --project="${PROJECT_ID}" --format='value(autoCreateSubnetworks)')" == False ]]
for spec in "${VM_SUBNET}:${VM_SUBNET_RANGE}" "${SERVERLESS_SUBNET}:${SERVERLESS_SUBNET_RANGE}"; do
  name="${spec%%:*}"; range="${spec#*:}"
  [[ "$(gcloud compute networks subnets describe "${name}" --project="${PROJECT_ID}" --region="${REGION}" --format='value(ipCidrRange)')" == "${range}" ]]
  [[ "$(gcloud compute networks subnets describe "${name}" --project="${PROJECT_ID}" --region="${REGION}" --format='value(privateIpGoogleAccess)')" == True ]]
done
gcloud compute ssh "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --tunnel-through-iap --quiet \
  --command="sudo python3 -c 'import json; assert json.load(open(\"/etc/discrawl/archive-api.json\"))[\"allowed_source_cidr\"] == \"${SERVERLESS_SUBNET_RANGE}\"'"
nat_json="$(gcloud compute routers nats describe "${nat}" --router="${router}" --project="${PROJECT_ID}" --region="${REGION}" --format=json)"
NAT_JSON="${nat_json}" python3 - "${VM_SUBNET}" <<'PY'
import json, os, sys
d=json.loads(os.environ["NAT_JSON"]); expected=sys.argv[1]
assert d.get("natIpAllocateOption") == "AUTO_ONLY"
assert d.get("sourceSubnetworkIpRangesToNat") == "LIST_OF_SUBNETWORKS"
assert d.get("logConfig", {}).get("enable") is True and d["logConfig"].get("filter") == "ERRORS_ONLY"
subs=d.get("subnetworks", [])
assert len(subs) == 1 and subs[0]["name"].endswith("/"+expected)
PY
disk_json="$(gcloud compute disks describe "${VM_NAME}-data" --project="${PROJECT_ID}" --zone="${ZONE}" --format=json)"
DISK_JSON="${disk_json}" python3 - "${VM_NAME}-weekly" <<'PY'
import json, os, sys
d=json.loads(os.environ["DISK_JSON"])
assert int(d["sizeGb"]) >= 30 and d["type"].endswith("/pd-standard")
assert any(x.endswith("/"+sys.argv[1]) for x in d.get("resourcePolicies", []))
PY
policy_json="$(gcloud compute resource-policies describe "${VM_NAME}-weekly" --project="${PROJECT_ID}" --region="${REGION}" --format=json)"
POLICY_JSON="${policy_json}" python3 - <<'PY'
import json, os
d=json.loads(os.environ["POLICY_JSON"])["snapshotSchedulePolicy"]
assert int(d["retentionPolicy"]["maxRetentionDays"]) == 14
assert d["retentionPolicy"]["onSourceDiskDelete"] == "KEEP_AUTO_SNAPSHOTS"
weekly=d["schedule"]["weeklySchedule"]["dayOfWeeks"]
assert len(weekly) == 1 and weekly[0]["day"] in ("SUN", "SUNDAY") and weekly[0]["startTime"] == "05:30"
PY

project_number="$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)')"
bucket_json="$(gcloud storage buckets describe "gs://${BACKUP_BUCKET}" --format=json)"
BUCKET_JSON="${bucket_json}" python3 - "${REGION}" "${project_number}" <<'PY'
import json, os, sys
d=json.loads(os.environ["BUCKET_JSON"]); region=sys.argv[1].upper(); project=sys.argv[2]
assert str(d.get("location", "")).upper() == region
assert str(d.get("projectNumber", d.get("project_number", ""))) == project
iam=d.get("iamConfiguration", d.get("iam_config", {}))
ubla=iam.get("uniformBucketLevelAccess", iam.get("uniform_bucket_level_access", iam.get("bucket_policy_only", {})))
assert ubla.get("enabled") is True
assert iam.get("publicAccessPrevention", iam.get("public_access_prevention")) == "enforced"
versioning=d.get("versioning", {})
assert d.get("versioning_enabled", versioning.get("enabled")) is True
soft=d.get("softDeletePolicy", d.get("soft_delete_policy", {}))
assert int(soft.get("retentionDurationSeconds", soft.get("retention_duration_seconds", 0))) == 604800
ret=d.get("retentionPolicy", d.get("retention_policy", {}))
assert int(ret.get("retentionPeriod", ret.get("retention_period", 0))) == 86400
life=d.get("lifecycle", d.get("lifecycle_config", {})).get("rule", [])
assert len(life) == 3
def action(rule): return rule.get("action", {}).get("type")
def cond(rule): return rule.get("condition", {})
assert any(action(r)=="Delete" and int(cond(r).get("age",0))==30 and "daily/" in cond(r).get("matchesPrefix", cond(r).get("matches_prefix", [])) for r in life)
assert any(action(r)=="Delete" and int(cond(r).get("age",0))==365 and "monthly/" in cond(r).get("matchesPrefix", cond(r).get("matches_prefix", [])) for r in life)
assert any(action(r)=="Delete" and int(cond(r).get("age",0))==1 and cond(r).get("isLive", cond(r).get("is_live")) is False for r in life)
PY

member="serviceAccount:${archive_sa}"
project_iam="$(gcloud projects get-iam-policy "${PROJECT_ID}" --format=json)"
PROJECT_IAM="${project_iam}" python3 - "${member}" <<'PY'
import json, os, sys
d=json.loads(os.environ["PROJECT_IAM"]); member=sys.argv[1]
roles={b["role"] for b in d.get("bindings", []) if member in b.get("members", [])}
required={"roles/datastore.user", "roles/firebasedatabase.admin", "roles/logging.logWriter", "roles/monitoring.metricWriter"}
assert roles == required
PY
ancestor_iam="$(gcloud projects get-ancestors-iam-policy "${PROJECT_ID}" --format=json)"
ANCESTOR_IAM="${ancestor_iam}" python3 - "${member}" <<'PY'
import json, os, sys
rows=json.loads(os.environ["ANCESTOR_IAM"]); member=sys.argv[1]
roles=set()
for row in rows:
    for binding in row.get("policy", {}).get("bindings", []):
        if member in binding.get("members", []):
            roles.add(binding["role"])
required={"roles/datastore.user", "roles/firebasedatabase.admin", "roles/logging.logWriter", "roles/monitoring.metricWriter"}
assert roles == required
PY
secret_iam="$(gcloud secrets get-iam-policy "${BOT_SECRET_ID}" --project="${PROJECT_ID}" --format=json)"
SECRET_IAM="${secret_iam}" python3 - "${member}" <<'PY'
import json, os, sys
d=json.loads(os.environ["SECRET_IAM"]); member=sys.argv[1]
roles={b["role"] for b in d.get("bindings", []) if member in b.get("members", [])}
assert roles == set()
PY
bucket_iam="$(gcloud storage buckets get-iam-policy "gs://${BACKUP_BUCKET}" --format=json)"
BUCKET_IAM="${bucket_iam}" python3 - "${member}" <<'PY'
import json, os, sys
d=json.loads(os.environ["BUCKET_IAM"]); member=sys.argv[1]
roles={b["role"] for b in d.get("bindings", []) if member in b.get("members", [])}
assert roles == {"roles/storage.objectCreator"}
PY

for rule in "${VM_NAME}-archive-private:${SERVERLESS_SUBNET_RANGE}:8787" "${VM_NAME}-iap-ssh:35.235.240.0/20:22"; do
  name="${rule%%:*}"; rest="${rule#*:}"; source="${rest%:*}"; port="${rest##*:}"
  firewall_json="$(gcloud compute firewall-rules describe "${name}" --project="${PROJECT_ID}" --format=json)"
  FIREWALL_JSON="${firewall_json}" python3 - "${VPC_NETWORK}" "${archive_sa}" "${source}" "${port}" <<'PY'
import json, os, sys
d=json.loads(os.environ["FIREWALL_JSON"]); network, sa, source, port=sys.argv[1:]
assert d["network"].endswith("/"+network) and d.get("direction") == "INGRESS"
assert d.get("disabled") is not True
assert d.get("sourceRanges") == [source] and d.get("targetServiceAccounts") == [sa]
assert d.get("allowed") == [{"IPProtocol":"tcp", "ports":[port]}]
PY
done
target_firewalls="$(gcloud compute firewall-rules list --project="${PROJECT_ID}" --format=json)"
TARGET_FIREWALLS="${target_firewalls}" python3 - "${VM_NAME}-archive-private" "${SERVERLESS_SUBNET_RANGE}" "${VPC_NETWORK}" "${archive_sa}" <<'PY'
import json, os, sys
rules=json.loads(os.environ["TARGET_FIREWALLS"]); expected, source, network, sa=sys.argv[1:]
port_8787=[]
def covers_port(spec, port):
    parts=str(spec).split("-", 1)
    try:
        first=int(parts[0]); last=int(parts[-1])
    except ValueError as exc:
        raise AssertionError(f"invalid firewall port specification: {spec}") from exc
    assert 1 <= first <= last <= 65535
    return first <= port <= last
def allows_tcp_port(allow, port):
    protocol=str(allow.get("IPProtocol", "")).lower()
    if protocol == "all":
        return True
    if protocol not in ("tcp", "6"):
        return False
    ports=allow.get("ports", [])
    return not ports or any(covers_port(spec, port) for spec in ports)
for rule in rules:
    if not rule.get("network", "").endswith("/"+network) or rule.get("disabled") is True or rule.get("direction") != "INGRESS":
        continue
    target_sas=rule.get("targetServiceAccounts", [])
    target_tags=rule.get("targetTags", [])
    # The archive VM is untagged. No target applies to every VM; an explicit
    # service-account target applies only when it names the archive identity.
    if target_tags or (target_sas and sa not in target_sas):
        continue
    for allow in rule.get("allowed", []):
        if allows_tcp_port(allow, 8787):
            port_8787.append(rule)
assert len(port_8787) == 1 and port_8787[0]["name"] == expected and port_8787[0].get("sourceRanges") == [source]
PY

ready=false
gcloud compute ssh "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --tunnel-through-iap --quiet \
  --command='set -euo pipefail; systemctl is-active --quiet systemd-resolved.service; test "$(readlink -f /etc/resolv.conf)" = /run/systemd/resolve/stub-resolv.conf; test "$(getent ahostsv4 metadata.google.internal | awk "NR == 1 {print \$1}")" = 169.254.169.254; curl --noproxy "*" --fail --silent --show-error --max-time 3 -H "Metadata-Flavor: Google" http://169.254.169.254/computeMetadata/v1/instance/id >/dev/null'
gcloud compute ssh "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --tunnel-through-iap --quiet \
  --command='set -euo pipefail; systemctl is-active --quiet systemd-resolved.service; test "$(readlink -f /etc/resolv.conf)" = /run/systemd/resolve/stub-resolv.conf; for service in discrawl-tail.service discrawl-sync.service; do effective="$(sudo systemctl show "$service" -p ProtectProc -p IPAddressDeny -p ExecStartPre)"; grep -q "ProtectProc=invisible" <<<"$effective"; grep -q "169.254.169.254" <<<"$effective"; grep -q "::ffff:169.254.169.254" <<<"$effective"; grep -q "fd20:ce::254" <<<"$effective"; grep -q "/usr/bin/test -x /usr/bin/curl" <<<"$effective"; grep -q "/usr/bin/getent ahosts discord.com" <<<"$effective"; grep -q -- "--noproxy" <<<"$effective"; grep -q "http://169.254.169.254/computeMetadata" <<<"$effective"; grep -q "::ffff:169.254.169.254" <<<"$effective"; grep -q "fd20:ce::254" <<<"$effective"; done; sudo systemd-run --quiet --wait --collect --unit=discrawl-tail-network-probe --property=User=discrawl-tail --property=Group=discrawl --property=ProtectProc=invisible --property="IPAddressDeny=169.254.169.254/32 ::ffff:169.254.169.254/128 fd20:ce::254/128" /bin/sh -c "getent ahosts discord.com >/dev/null && ! curl --noproxy \"*\" --fail --silent --show-error --max-time 3 -H \"Metadata-Flavor: Google\" http://169.254.169.254/computeMetadata/v1/instance/service-accounts/default/token && ! curl --noproxy \"*\" --fail --silent --show-error --max-time 3 -g -H \"Metadata-Flavor: Google\" \"http://[::ffff:169.254.169.254]/computeMetadata/v1/instance/service-accounts/default/token\"" >/dev/null'
for _ in $(seq 1 30); do
  if gcloud compute ssh "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --tunnel-through-iap --quiet \
    --command='curl --fail --silent --show-error --max-time 10 http://127.0.0.1:8787/readyz >/dev/null; test "$(curl --silent --output /dev/null --write-out "%{http_code}" --max-time 10 http://127.0.0.1:8787/v1/status)" = 401; sudo systemctl is-active --quiet discrawl-tail discrawl-api backup-discrawl.timer' \
    >/dev/null 2>&1; then
    ready=true
    break
  fi
  sleep 10
done
[[ "${ready}" == true ]] || { echo "archive services did not become ready" >&2; exit 1; }
token="$(gcloud auth print-identity-token --impersonate-service-account="${CLOUD_RUN_CALLER_SERVICE_ACCOUNT}" --audiences="${ARCHIVE_AUDIENCE}")"
status_json="$(printf '%s\n' "${token}" | gcloud compute ssh "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" \
  --tunnel-through-iap --quiet --command='IFS= read -r token; curl --fail --silent --show-error --max-time 10 -H "Authorization: Bearer ${token}" http://127.0.0.1:8787/v1/status')"
unset token
STATUS_JSON="${status_json}" python3 - "${DISCORD_GUILD_ID}" <<'PY'
import json, os, sys
d=json.loads(os.environ["STATUS_JSON"])
assert d.get("guild_id") == sys.argv[1]
assert isinstance(d.get("stale"), bool) and isinstance(d.get("degraded"), bool)
assert d["stale"] is False and d["degraded"] is False
PY

# A healthy API is insufficient for cutover. Read the external projection
# checkpoint until initial bindings and both exhaustive privacy sweeps finish.
projection_url="https://firestore.googleapis.com/v1/projects/${PROJECT_ID}/databases/(default)/documents/orgs/${ORG_ID}/chatRuntime/discrawlProjection"
projection_healthy=false
for _ in $(seq 1 90); do
  access_token="$(gcloud auth print-access-token)"
  projection_json="$(curl --fail --silent --show-error --max-time 15 -H "Authorization: Bearer ${access_token}" "${projection_url}" 2>/dev/null || true)"
  unset access_token
  if PROJECTION_JSON="${projection_json}" python3 - "${DISCORD_GUILD_ID}" "${verify_started}" <<'PY'
import json, os, sys
from datetime import datetime, timezone, timedelta
try:
    f=json.loads(os.environ["PROJECTION_JSON"])["fields"]
    stamp=datetime.fromisoformat(f["lastSuccessAt"]["timestampValue"].replace("Z", "+00:00"))
    started=datetime.fromisoformat(sys.argv[2].replace("Z", "+00:00"))
    age=datetime.now(timezone.utc)-stamp
    ok=(f["state"]["stringValue"] == "healthy" and
        f["schemaVersion"]["integerValue"] == "2" and
        f["guildId"]["stringValue"] == sys.argv[1] and
        f["tombstoneSweepComplete"]["booleanValue"] is True and
        f["attachmentUrlSweepComplete"]["booleanValue"] is True and
        stamp >= started and -timedelta(minutes=20) <= age <= timedelta(minutes=20))
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
[[ "${projection_healthy}" == true ]] || { echo "Firestore projection did not finish seeding, tombstone sweep, and attachment URL sweep" >&2; exit 1; }
echo "verified isolated private Discrawl deployment for ${PROJECT_ID}/${VM_NAME}"
