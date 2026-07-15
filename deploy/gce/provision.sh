#!/usr/bin/env bash
set -euo pipefail
umask 077

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
required=(PROJECT_ID REGION ZONE VM_NAME ARCHIVE_SERVICE_ACCOUNT BACKUP_BUCKET BOT_SECRET_ID \
  VPC_NETWORK VM_SUBNET VM_SUBNET_RANGE SERVERLESS_SUBNET SERVERLESS_SUBNET_RANGE ARCHIVE_INTERNAL_IP \
  CLOUD_RUN_CALLER_SERVICE_ACCOUNT DISCORD_GUILD_ID ORG_ID DATABASE_URL ARCHIVE_AUDIENCE \
  DISCRAWL_TARBALL DISCRAWL_TARBALL_SHA256)
for name in "${required[@]}"; do
  [[ -n "${!name:-}" ]] || { echo "missing ${name}" >&2; exit 1; }
done
[[ -f "${DISCRAWL_TARBALL}" ]] || { echo "release archive not found" >&2; exit 1; }
printf '%s  %s\n' "${DISCRAWL_TARBALL_SHA256}" "${DISCRAWL_TARBALL}" | sha256sum --check --status || {
  echo "release archive checksum mismatch" >&2; exit 1;
}
[[ "${CLOUD_RUN_CALLER_SERVICE_ACCOUNT}" == *@"${PROJECT_ID}".iam.gserviceaccount.com ]] || {
  echo "caller service account must belong to tenant project" >&2; exit 1;
}
[[ "${PROJECT_ID}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ && "${ORG_ID}" =~ ^[A-Za-z0-9_-]{1,128}$ ]] || {
  echo "invalid project or org identifier" >&2; exit 1;
}
[[ "${ARCHIVE_SERVICE_ACCOUNT}" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ && "${DISCORD_GUILD_ID}" =~ ^[0-9]{17,20}$ ]] || {
  echo "invalid service-account name or guild snowflake" >&2; exit 1;
}
[[ "${ARCHIVE_AUDIENCE}" =~ ^https://[A-Za-z0-9._:/-]+$ ]] || { echo "archive audience must be an exact query-free HTTPS URL" >&2; exit 1; }
[[ "${DATABASE_URL}" == "https://${PROJECT_ID}-default-rtdb.firebaseio.com" ]] || { echo "database URL must be tenant default RTDB origin" >&2; exit 1; }

python3 - "${VM_SUBNET_RANGE}" "${SERVERLESS_SUBNET_RANGE}" "${ARCHIVE_INTERNAL_IP}" <<'PY'
import ipaddress, sys
vm, serverless = map(ipaddress.ip_network, sys.argv[1:3])
ip = ipaddress.ip_address(sys.argv[3])
private = [ipaddress.ip_network("10.0.0.0/8"), ipaddress.ip_network("172.16.0.0/12"), ipaddress.ip_network("192.168.0.0/16")]
if not any(vm.subnet_of(block) for block in private) or not any(serverless.subnet_of(block) for block in private) or not 24 <= serverless.prefixlen <= 26:
    raise SystemExit("subnets must be RFC1918; Direct VPC subnet must be /24, /25, or /26")
if vm.overlaps(serverless) or ip not in vm or ip in (vm.network_address, vm.broadcast_address):
    raise SystemExit("subnets must not overlap and internal IP must be in VM subnet")
PY

archive_sa="${ARCHIVE_SERVICE_ACCOUNT}@${PROJECT_ID}.iam.gserviceaccount.com"
router="${VM_NAME}-router"
nat="${VM_NAME}-public-nat"
snapshot_policy="${VM_NAME}-weekly"

wait_for_iap_ssh() {
  local attempt
  for attempt in $(seq 1 30); do
    if gcloud compute ssh "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" \
      --tunnel-through-iap --quiet --command=true >/dev/null 2>&1; then
      return 0
    fi
    sleep 10
  done
  echo "VM did not become reachable through IAP SSH" >&2
  return 1
}

required_services=(compute.googleapis.com firebasedatabase.googleapis.com firestore.googleapis.com \
  storage.googleapis.com logging.googleapis.com monitoring.googleapis.com secretmanager.googleapis.com)
if [[ "${DISCRAWL_APIS_PRE_ENABLED:-false}" == true ]]; then
  enabled_services="$(gcloud services list --enabled --project="${PROJECT_ID}" --format='value(config.name)')"
  for service in "${required_services[@]}"; do
    grep -Fxq "${service}" <<<"${enabled_services}" || {
      echo "required pre-enabled API is missing: ${service}" >&2
      exit 1
    }
  done
else
  gcloud services enable --project="${PROJECT_ID}" "${required_services[@]}"
fi

# Remove any public IPv4 access configuration before performing slower
# convergence work. This also repairs instances created by older gcloud
# invocations where a separate --no-address was overridden by
# --network-interface.
if gcloud compute instances describe "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" >/dev/null 2>&1; then
  instance_json="$(gcloud compute instances describe "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --format=json)"
  public_access_rows="$(INSTANCE_JSON="${instance_json}" python3 - <<'PY'
import json, os
for nic in json.loads(os.environ["INSTANCE_JSON"]).get("networkInterfaces", []):
    for access in nic.get("accessConfigs", []):
        print(f"{nic['name']}\t{access['name']}")
PY
)"
  while IFS=$'\t' read -r nic_name access_name; do
    [[ -n "${nic_name}" && -n "${access_name}" ]] || continue
    gcloud compute instances delete-access-config "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" \
      --network-interface="${nic_name}" --access-config-name="${access_name}" --quiet
  done <<<"${public_access_rows}"
fi

if ! gcloud compute networks describe "${VPC_NETWORK}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud compute networks create "${VPC_NETWORK}" --project="${PROJECT_ID}" --subnet-mode=custom
fi
[[ "$(gcloud compute networks describe "${VPC_NETWORK}" --project="${PROJECT_ID}" --format='value(autoCreateSubnetworks)')" == False ]] || {
  echo "network drift: ${VPC_NETWORK} is not custom mode" >&2; exit 1;
}

ensure_subnet() {
  local name="$1" range="$2"
  if ! gcloud compute networks subnets describe "${name}" --project="${PROJECT_ID}" --region="${REGION}" >/dev/null 2>&1; then
    gcloud compute networks subnets create "${name}" --project="${PROJECT_ID}" --region="${REGION}" \
      --network="${VPC_NETWORK}" --range="${range}" --enable-private-ip-google-access
  fi
  [[ "$(gcloud compute networks subnets describe "${name}" --project="${PROJECT_ID}" --region="${REGION}" --format='value(ipCidrRange)')" == "${range}" ]] || {
    echo "subnet drift: ${name} range" >&2; exit 1;
  }
  [[ "$(gcloud compute networks subnets describe "${name}" --project="${PROJECT_ID}" --region="${REGION}" --format='value(network.basename())')" == "${VPC_NETWORK}" ]] || {
    echo "subnet drift: ${name} network" >&2; exit 1;
  }
}
ensure_subnet "${VM_SUBNET}" "${VM_SUBNET_RANGE}"
ensure_subnet "${SERVERLESS_SUBNET}" "${SERVERLESS_SUBNET_RANGE}"

if ! gcloud compute routers describe "${router}" --project="${PROJECT_ID}" --region="${REGION}" >/dev/null 2>&1; then
  gcloud compute routers create "${router}" --project="${PROJECT_ID}" --region="${REGION}" --network="${VPC_NETWORK}"
fi
if ! gcloud compute routers nats describe "${nat}" --router="${router}" --project="${PROJECT_ID}" --region="${REGION}" >/dev/null 2>&1; then
  gcloud compute routers nats create "${nat}" --router="${router}" --project="${PROJECT_ID}" --region="${REGION}" \
    --nat-custom-subnet-ip-ranges="${VM_SUBNET}" --auto-allocate-nat-external-ips --enable-logging --log-filter=ERRORS_ONLY
fi
[[ "$(gcloud compute routers describe "${router}" --project="${PROJECT_ID}" --region="${REGION}" --format='value(network.basename())')" == "${VPC_NETWORK}" ]] || {
  echo "router drift" >&2; exit 1;
}

if ! gcloud compute addresses describe "${VM_NAME}-internal" --project="${PROJECT_ID}" --region="${REGION}" >/dev/null 2>&1; then
  gcloud compute addresses create "${VM_NAME}-internal" --project="${PROJECT_ID}" --region="${REGION}" \
    --subnet="${VM_SUBNET}" --addresses="${ARCHIVE_INTERNAL_IP}"
fi
[[ "$(gcloud compute addresses describe "${VM_NAME}-internal" --project="${PROJECT_ID}" --region="${REGION}" --format='value(address)')" == "${ARCHIVE_INTERNAL_IP}" ]] || {
  echo "internal address drift" >&2; exit 1;
}

if ! gcloud iam service-accounts describe "${archive_sa}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud iam service-accounts create "${ARCHIVE_SERVICE_ACCOUNT}" --project="${PROJECT_ID}" --display-name="Tenant Discrawl archive"
fi
for role in roles/datastore.user roles/firebasedatabase.admin roles/logging.logWriter roles/monitoring.metricWriter; do
  gcloud projects add-iam-policy-binding "${PROJECT_ID}" --member="serviceAccount:${archive_sa}" --role="${role}" \
    --condition=None --quiet >/dev/null
done
# Converge away from privileges granted by pre-hardening drafts. Secret access
# remains with the provisioning operator; backups are append-only from the VM.
gcloud projects remove-iam-policy-binding "${PROJECT_ID}" --member="serviceAccount:${archive_sa}" \
  --role=roles/secretmanager.secretAccessor --condition=None --quiet >/dev/null 2>&1 || true
gcloud secrets remove-iam-policy-binding "${BOT_SECRET_ID}" --project="${PROJECT_ID}" \
  --member="serviceAccount:${archive_sa}" --role=roles/secretmanager.secretAccessor \
  --condition=None --quiet >/dev/null 2>&1 || true

if ! gcloud storage buckets describe "gs://${BACKUP_BUCKET}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
  gcloud storage buckets create "gs://${BACKUP_BUCKET}" --project="${PROJECT_ID}" --location="${REGION}" --uniform-bucket-level-access
fi
project_number="$(gcloud projects describe "${PROJECT_ID}" --format='value(projectNumber)')"
bucket_json="$(gcloud storage buckets describe "gs://${BACKUP_BUCKET}" --raw --format=json)"
BUCKET_JSON="${bucket_json}" python3 - "${REGION}" "${project_number}" <<'PY'
import json, os, sys
d=json.loads(os.environ["BUCKET_JSON"]); region=sys.argv[1].upper(); project=sys.argv[2]
if str(d.get("location", "")).upper() != region:
    raise SystemExit("backup bucket region drift")
if str(d.get("projectNumber", d.get("project_number", ""))) != project:
    raise SystemExit("backup bucket project drift")
PY
gcloud storage buckets update "gs://${BACKUP_BUCKET}" --pap --versioning \
  --soft-delete-duration=7d --retention-period=1d --lifecycle-file="${SCRIPT_DIR}/backup-lifecycle.json"
gcloud storage buckets add-iam-policy-binding "gs://${BACKUP_BUCKET}" \
  --member="serviceAccount:${archive_sa}" --role=roles/storage.objectCreator \
  --condition=None --quiet >/dev/null
gcloud storage buckets remove-iam-policy-binding "gs://${BACKUP_BUCKET}" \
  --member="serviceAccount:${archive_sa}" --role=roles/storage.objectAdmin \
  --condition=None --quiet >/dev/null 2>&1 || true

if ! gcloud compute disks describe "${VM_NAME}-data" --project="${PROJECT_ID}" --zone="${ZONE}" >/dev/null 2>&1; then
  gcloud compute disks create "${VM_NAME}-data" --project="${PROJECT_ID}" --zone="${ZONE}" --type=pd-standard --size=30GB
fi
[[ "$(gcloud compute disks describe "${VM_NAME}-data" --project="${PROJECT_ID}" --zone="${ZONE}" --format='value(sizeGb)')" -ge 30 ]] || {
  echo "data disk drift" >&2; exit 1;
}
if ! gcloud compute resource-policies describe "${snapshot_policy}" --project="${PROJECT_ID}" --region="${REGION}" >/dev/null 2>&1; then
  gcloud compute resource-policies create snapshot-schedule "${snapshot_policy}" --project="${PROJECT_ID}" --region="${REGION}" \
    --weekly-schedule=sunday --start-time=05:30 --max-retention-days=14 --on-source-disk-delete=keep-auto-snapshots
fi
attached_policies="$(gcloud compute disks describe "${VM_NAME}-data" --project="${PROJECT_ID}" --zone="${ZONE}" --format='value(resourcePolicies.list())')"
if [[ ",${attached_policies}," != *",${snapshot_policy},"* ]]; then
  gcloud compute disks add-resource-policies "${VM_NAME}-data" --project="${PROJECT_ID}" --zone="${ZONE}" \
    --resource-policies="${snapshot_policy}"
fi

if ! gcloud compute instances describe "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" >/dev/null 2>&1; then
  gcloud compute instances create "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" \
    --machine-type=e2-medium --image-family=debian-12 --image-project=debian-cloud \
    --boot-disk-type=pd-standard --boot-disk-size=10GB \
    --disk="name=${VM_NAME}-data,device-name=discrawl-data,mode=rw,boot=no,auto-delete=no" \
    --service-account="${archive_sa}" --scopes=cloud-platform --deletion-protection \
    --network-interface="network=${VPC_NETWORK},subnet=${VM_SUBNET},private-network-ip=${ARCHIVE_INTERNAL_IP},no-address" \
    --metadata=block-project-ssh-keys=true,enable-oslogin=TRUE
fi
instance_json="$(gcloud compute instances describe "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --format=json)"
INSTANCE_JSON="${instance_json}" python3 - "${VPC_NETWORK}" "${VM_SUBNET}" "${ARCHIVE_INTERNAL_IP}" "${archive_sa}" <<'PY'
import json, os, sys
d=json.loads(os.environ["INSTANCE_JSON"]); network, subnet, ip, sa=sys.argv[1:]
assert len(d["networkInterfaces"]) == 1
assert not d.get("tags", {}).get("items")
nic=d['networkInterfaces'][0]
assert nic['network'].endswith('/'+network) and nic['subnetwork'].endswith('/'+subnet)
assert nic['networkIP']==ip and not nic.get('accessConfigs') and not nic.get('ipv6AccessConfigs')
assert d.get('deletionProtection') is True
assert d['serviceAccounts'][0]['email']==sa
PY

# Provisioning is also the upgrade path. Migrations and broad repair syncs run
# offline with bounded headroom, then the host always returns to e2-micro.
machine_type="$(gcloud compute instances describe "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --format='value(machineType.basename())')"
if [[ "${machine_type}" != e2-medium ]]; then
  gcloud compute instances stop "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --quiet
  gcloud compute instances set-machine-type "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --machine-type=e2-medium
  gcloud compute instances start "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --quiet
fi

ensure_firewall() {
  local name="$1" rules="$2" sources="$3" firewall_json
  if ! gcloud compute firewall-rules describe "${name}" --project="${PROJECT_ID}" >/dev/null 2>&1; then
    gcloud compute firewall-rules create "${name}" --project="${PROJECT_ID}" --network="${VPC_NETWORK}" \
      --direction=INGRESS --action=ALLOW --rules="${rules}" --source-ranges="${sources}" \
      --target-service-accounts="${archive_sa}"
  fi
  firewall_json="$(gcloud compute firewall-rules describe "${name}" --project="${PROJECT_ID}" --format=json)"
  FIREWALL_JSON="${firewall_json}" python3 - "${VPC_NETWORK}" "${sources}" "${archive_sa}" "${rules#tcp:}" <<'PY'
import json, os, sys
d=json.loads(os.environ["FIREWALL_JSON"]); network, source, sa, port=sys.argv[1:]
assert d.get("network", "").endswith("/"+network)
assert d.get("sourceRanges") == [source]
assert d.get("targetServiceAccounts") == [sa]
assert d.get("disabled") is not True and d.get("direction") == "INGRESS"
assert d.get("allowed") == [{"IPProtocol":"tcp", "ports":[port]}]
PY
}
ensure_firewall "${VM_NAME}-archive-private" tcp:8787 "${SERVERLESS_SUBNET_RANGE}"
ensure_firewall "${VM_NAME}-iap-ssh" tcp:22 35.235.240.0/20

stage="$(mktemp -d)"
trap 'rm -rf "${stage}"' EXIT
cp "${DISCRAWL_TARBALL}" "${stage}/discrawl-release.tar.gz"
printf '%s  %s\n' "${DISCRAWL_TARBALL_SHA256}" discrawl-release.tar.gz > "${stage}/discrawl-release.sha256"
cp "${SCRIPT_DIR}/install-host.sh" "${SCRIPT_DIR}/restore-discrawl.sh" "${SCRIPT_DIR}/backup-discrawl.sh" \
  "${SCRIPT_DIR}/discrawl-tail.service" "${SCRIPT_DIR}/discrawl-sync.service" "${SCRIPT_DIR}/discrawl-api.service" \
  "${SCRIPT_DIR}/backup-discrawl.service" "${SCRIPT_DIR}/backup-discrawl.timer" "${stage}/"
sed -e "s|%%DISCORD_GUILD_ID%%|${DISCORD_GUILD_ID}|g" "${SCRIPT_DIR}/config.toml.template" > "${stage}/config.toml"
sed -e "s|%%DISCORD_GUILD_ID%%|${DISCORD_GUILD_ID}|g" \
  -e "s|%%ARCHIVE_AUDIENCE%%|${ARCHIVE_AUDIENCE}|g" \
  -e "s|%%CLOUD_RUN_CALLER_SERVICE_ACCOUNT%%|${CLOUD_RUN_CALLER_SERVICE_ACCOUNT}|g" \
  -e "s|%%SERVERLESS_SUBNET_RANGE%%|${SERVERLESS_SUBNET_RANGE}|g" \
  -e "s|%%PROJECT_ID%%|${PROJECT_ID}|g" -e "s|%%ORG_ID%%|${ORG_ID}|g" \
  -e "s|%%DATABASE_URL%%|${DATABASE_URL}|g" "${SCRIPT_DIR}/archive-api.json.template" > "${stage}/archive-api.json"
token="$(gcloud secrets versions access latest --secret="${BOT_SECRET_ID}" --project="${PROJECT_ID}")"
[[ "${token}" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bot secret has unsafe format" >&2; exit 1; }
printf 'DISCORD_BOT_TOKEN=%s\n' "${token}" > "${stage}/bot-token.env"
unset token
printf 'DISCRAWL_BACKUP_BUCKET=%s\n' "${BACKUP_BUCKET}" > "${stage}/backup.env"
printf 'DISCORD_GUILD_ID=%s\n' "${DISCORD_GUILD_ID}" > "${stage}/install.env"

remote_stage="/tmp/discrawl-install-$(date -u +%Y%m%dT%H%M%SZ)-$$"
wait_for_iap_ssh
gcloud compute scp --recurse "${stage}" "${VM_NAME}:${remote_stage}" --project="${PROJECT_ID}" --zone="${ZONE}" --tunnel-through-iap --quiet
gcloud compute ssh "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --tunnel-through-iap \
  --command="rc=0; sudo '${remote_stage}/install-host.sh' || rc=\$?; cleanup=0; sudo rm -rf -- '${remote_stage}' || cleanup=\$?; if [[ \$rc -ne 0 ]]; then exit \$rc; fi; exit \$cleanup"

# Full history plus schema normalization is a bounded bootstrap/upgrade job, not
# an e2-micro workload. New hosts bootstrap at e2-medium, then converge to the
# steady-state size only after the installer and integrity checks succeed.
machine_type="$(gcloud compute instances describe "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --format='value(machineType.basename())')"
if [[ "${machine_type}" != e2-micro ]]; then
  gcloud compute instances stop "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --quiet
  gcloud compute instances set-machine-type "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --machine-type=e2-micro
  gcloud compute instances start "${VM_NAME}" --project="${PROJECT_ID}" --zone="${ZONE}" --quiet
fi
wait_for_iap_ssh

"${SCRIPT_DIR}/verify-deployment.sh"
