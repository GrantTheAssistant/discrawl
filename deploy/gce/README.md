# Isolated GCE archive service

This package deploys the same pinned Discrawl release into two separate tenant
projects. Each tenant owns a VM, persistent disk, service account, bot secret,
backup bucket, VPC, subnets, NAT, audience, caller identity, SQLite archive, and
Firestore deletion ledger. Never share any one of those resources across
`brennos` and `lazarteam-tools`.

## Data-plane contract

- `discrawl-tail` is the only SQLite writer. SQLite/FTS is canonical Discord
  history and search; Firestore remains the product auth/send/realtime plane.
- `discrawl-api` is a private read API and bounded Firestore projector. It
  accepts a Google ID token only for one exact HTTPS audience and one exact
  tenant Cloud Run service-account email.
- The VM has no external address. Discord egress uses Public Cloud NAT on the
  VM subnet. Cloud Run reaches port 8787 through its dedicated Direct VPC
  egress subnet (`private-ranges-only`), not a Serverless VPC Access connector.
- Attachment CDN URLs are never projected or returned by the archive API.
  Products hydrate current descriptors transiently from Discord and enforce
  exact host/path/signature/expiry checks.
- `orgs/{org}/chatTombstones/{messageId}` is the append-only terminal deletion
  ledger. It is intentionally outside SQLite, identity-free, and survives a DB
  restore. Product rules deny all client access.

## Build and provision

Build one signed release containing both `discrawl` and `discrawl-api`, record
its SHA-256 digest, then run `provision.sh` once per project with the required
environment variables. Use different CIDRs, internal IPs, audiences, caller
service accounts, secrets, buckets, and names for each invocation. The script
is convergent: it creates or verifies resources, removes legacy broad VM IAM,
performs the initial full/member sync, and calls `verify-deployment.sh`.

The provisioning operator—not the runtime SA—must be able to enable APIs and
create/read/update IAM, Compute/VPC/NAT/firewall/disk/snapshot, Storage, and
service-account resources in the tenant project; read the exact bot secret;
use IAP TCP forwarding plus OS Admin Login/sudo on the VM; read Firestore; and
mint an audience-bound ID token as the exact Cloud Run caller SA. In practice,
grant `roles/iam.serviceAccountTokenCreator` on that caller SA and the relevant
IAP/OS Login roles to the operator. Keep these operator permissions off the
archive runtime SA.

When every required API is already enabled and the operator intentionally lacks
`serviceusage.services.enable`, set `DISCRAWL_APIS_PRE_ENABLED=true`.
Provisioning then performs an exact enabled-service readback and fails before
creating resources if any required API is missing. This flag does not bypass
any Compute, IAM, Secret Manager, IAP, OS Login, Storage, or verification
permission.

The VM service account has project-wide `roles/datastore.user`, plus
logging/metrics writer roles and `storage.objectCreator` on its tenant backup
bucket. Google Cloud IAM cannot scope the Firestore role to a collection, so
the separate tenant project remains the security boundary. Content-free chat
invalidation ticks live in a dedicated Firestore subcollection; the VM has no
Realtime Database or Secret Manager access and cannot read, overwrite, or
delete backups.
The provisioning operator reads the bot secret once and installs a mode-0400
environment file owned only by `discrawl-tail`.
Deployment verification reads the project and every ancestor IAM policy and
fails closed unless direct bindings for the archive service account resolve to
exactly the three documented project roles. The operator therefore needs IAM
policy read access on every ancestor. That readback cannot expand Google Group
or principal-set membership; before production approval, use Cloud Asset IAM
analysis with group expansion (or an equivalent organization-level review) to
prove the archive service account is not indirectly included in a broader
binding.

The custom VPC has two non-overlapping RFC1918 subnets:

- a VM subnet, included in Cloud NAT, for Discord/Google API egress;
- a dedicated `/24`, `/25`, or `/26` Direct VPC subnet, excluded from NAT, used by the
  tenant Cloud Run runtime to call the VM's fixed private IP.

The API rejects authenticated-route traffic outside that exact subnet before
OIDC verification and uses one fixed pre-authentication rate bucket per subnet
address. Forwarding headers are ignored. The fixed `/24` maximum keeps limiter
state bounded while preventing one sibling workload address from starving the
authorized Cloud Run caller's address.

Firewall ingress is limited to port 8787 from that exact Direct VPC CIDR and
port 22 from the IAP TCP-forwarding CIDR, both targeting only the archive VM
service account. Do not add public DNS, an external IP, or `0.0.0.0/0` ingress.

## Host boundaries and updates

The 30 GB or larger `pd-standard` data disk mounts at `/var/lib/discrawl` by
UUID. Three unprivileged users share a read group but not ownership:

- `discrawl-tail` owns the DB and writer/cache directories;
- `discrawl-api` can read SQLite and write only projection state;
- `discrawl-backup` can read SQLite and write only the backup staging dir.

Those Unix users separate filesystem ownership only. GCE attaches IAM to the
VM, not to a Unix UID: the API/projector and backup intentionally retain VM
service-account metadata access for Firestore and GCS. The Discord-facing
tail and offline sync units instead deny metadata IPv4 and IPv6, hide other
processes with `ProtectProc=invisible`, and fail closed unless Discord DNS works
through the local systemd-resolved stub while a metadata token probe fails.

The host also enables a root-owned 2 GB swapfile and enforces systemd memory
ceilings plus Go memory limits. Launch configuration disables attachment text
and media downloads and caps any later opt-in attachment at 10 MiB; attachment
text extraction must be capacity-tested because it adds variable NAT, CPU, and
SQLite storage cost.

An update must contain both binaries from the same release digest. Keep the old
binary and DB until `/readyz`, authenticated `/v1/status`, projection parity,
and a real bound-channel read pass. Binary rollback and DB rollback are
separate decisions; never run an older binary against a newer schema without
checking compatibility. Migration v5 normalizes every historical tombstone in
memory, so temporarily resize very large archives to a larger VM before that
upgrade, then return to `e2-micro` after verification.
The installer retains the immediately preceding binaries and configs under
`/var/lib/discrawl/releases/<UTC timestamp>` with checksums. Use them only when
the retained DB/schema is compatible; otherwise restore the matching DB backup
as a separate rollback decision.
It also retains the prior root-only runtime environment and unit/script set so
an invalid credential rotation can restore the actually working release. Treat
that directory as secret-bearing and delete it after the new release's external
verification window.
Its pre-upgrade headroom check uses the same worst-case two-copy allowance as
nightly backup because the checked raw rollback DB and its gzip output briefly
coexist.

## Backups and restore

The nightly timer uses SQLite `.backup`, runs `pragma quick_check`, gzip
compresses the result, creates a SHA-256 manifest, and uploads each object with
generation-match zero. Daily objects live for 30 days; the first backup of each
month also lives under `monthly/` for 365 days. Bucket versioning, seven-day
soft delete, and a noncurrent-version lifecycle bound accidental replacements.
Because the VM is append-only, a separate tenant restore operator needs
`storage.objectViewer` to download a chosen `.db.gz` and `.sha256` pair.

Stop and temporarily resize the VM to `e2-medium` or larger, start it, then run:

```sh
sudo restore-discrawl /tmp/archive-YYYYMMDDTHHMMSSZ.db.gz \
  /tmp/archive-YYYYMMDDTHHMMSSZ.db.gz.sha256
```

Restore verifies checksum, SQLite integrity, and exact guild scope before it
stops services. It moves the DB/WAL/SHM together into a timestamped rollback
directory, installs the restored DB, keeps the API offline for an all-channel
Discord catch-up, clears only projection cursor state, then starts tail and
API. Any failure automatically restores the prior DB trio. The independent
Firestore terminal-deletion ledger prevents restored messages from
resurrecting. Afterward rerun `verify-deployment.sh` and inspect projection
health, return the VM to `e2-micro`, and rerun verification before deleting the
rollback copy. Restore refuses to run below 3 GB RAM and caps the offline Go
process at 2 GiB.

## Cutover and rollback

1. First deploy this product code in both tenants with
   `DISCORD_ARCHIVE_PROJECTION_MODE=disabled` and the old ownership topology.
   Confirm every Gateway, REST-history, edit, and website-send writer stores
   attachment metadata only while live responses hydrate URLs transiently.
   Do not start or accept an attachment sweep from an older URL-persisting
   product revision.
2. Only after those metadata-only writers are live, install/reset/start the
   projector. Require complete initial seeding,
   exact `schemaVersion=2`, `tombstoneSweepComplete=true`, and
   `attachmentUrlSweepComplete=true` for a
   fresh exact-guild checkpoint. The attachment sweep is restart-safe and
   transactionally re-reads each page before scrubbing URL fields.
3. Deploy with `DISCORD_ARCHIVE_PROJECTION_MODE=shadow`. Keep the tenant's
   prior ownership topology authoritative: Lazar keeps its Gateway enabled;
   TeamBrenno remains Gateway-disabled and uses its existing REST-heal/cache
   path. Compare exact archive pages only when status has the correct guild and
   is neither stale nor degraded.
4. Check recent website-origin lag-overlay behavior, pagination, search,
   deletes, attachments, and a restore drill in each tenant. Then atomically
   deploy `DISCORD_GATEWAY_ENABLED=false` and
   `DISCORD_ARCHIVE_PROJECTION_MODE=authoritative`, with the exact private URL,
   audience, caller identity, VPC network, and Direct VPC subnet. Deployment
   readback must show min instances 0, CPU throttling enabled, private-ranges
   egress, and no connector.
5. Retain the old topology/config for rollback, but keep the new metadata-only
   writer code. If archive health fails, atomically
   restore the tenant's exact prior topology plus archive disabled, which
   clears the Cloud Run network attachment: Lazar returns to Gateway enabled,
   min 1, CPU unthrottled; TeamBrenno returns to Gateway disabled, min 0, CPU
   throttled. Never invent a second TeamBrenno Gateway during rollback. If an
   older URL-persisting revision is ever restored, invalidate/delete projection
   cursor state and rerun the attachment sweep before shadow or authoritative.

## Capacity, alerts, and cost

Alert when data-disk usage reaches 25% (warning) and 30% (critical), or when
computed backup/restore headroom is insufficient. Nightly backup's worst-case
raw-plus-gzip overlap requires roughly 3.2× the maximum live DB plus WAL;
restore itself requires roughly 2.2× plus WAL and margin. Also alert on
API/tail restart loops, stale/degraded or
wrong-guild status, incomplete seeding/tombstone/attachment-URL sweeps, backup age over 26
hours, and failed authenticated probes. Before resizing compare filesystem use,
`page_count * page_size`, WAL, compressed backup, and snapshot sizes. Resize
the PD online, grow ext4, run `quick_check`, verify capacity, and take a backup;
provision accepts any data disk at least 30 GB. Backup and restore scripts
preflight actual free bytes: backup reserves two uncompressed-copy equivalents
plus WAL and a 20% (at least 512 MiB) allowance, while restore reserves one copy
plus the same allowance. Restore moves its staged DB atomically on the same
filesystem instead of creating a third full copy.

At July 2026 list prices, the fixed two-tenant baseline is approximately
**$17.34/month** if one `e2-micro` receives the aggregate Compute Free Tier, or
**$24.65/month** with no compute Free Tier. This includes two single-zone
`e2-micro` VMs, two 10 GB standard boot disks, two 30 GB standard data disks,
two Public Cloud NAT gateway-hours, and their two public IPv4 addresses. It
excludes transient `e2-medium` upgrade/restore time, NAT processing, Internet/cross-zone egress, Firestore projection
reads/writes (including one durable ledger write per delete and content-free
invalidation ticks), logs,
snapshots, and backup bytes. Backup budgeting uses 42 live copies plus roughly
eight copy-equivalents for noncurrent/lifecycle lag and seven-day soft delete:
about **50× raw DB**, or **$1.00 per live DB GB per tenant-month** before
compression. Two 5 GB raw archives are therefore about $10/month before compression. Check
the current GCP calculator and the actual billing export before approval.

The one-time attachment sanitation is usage-based, not fixed baseline. For `N`
message documents and `U` URL-bearing documents it bills about `2N` reads (page
query plus transaction re-read) and `U` writes. At representative
$0.06/100k reads and $0.18/100k writes, 100k all-dirty documents cost roughly
$0.30 per tenant; actual regional pricing and dirty-row count vary.
