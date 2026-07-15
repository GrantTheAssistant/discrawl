package gce

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func readAsset(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func pythonHeredocAfter(t *testing.T, asset, marker string) string {
	t.Helper()
	markerAt := strings.Index(asset, marker)
	if markerAt < 0 {
		t.Fatalf("asset missing Python marker %q", marker)
	}
	heredocAt := strings.Index(asset[markerAt:], "<<'PY'\n")
	if heredocAt < 0 {
		t.Fatalf("asset marker %q has no Python heredoc", marker)
	}
	start := markerAt + heredocAt + len("<<'PY'\n")
	end := strings.Index(asset[start:], "\nPY\n")
	if end < 0 {
		t.Fatalf("asset marker %q has unterminated Python heredoc", marker)
	}
	return asset[start : start+end]
}

func runPythonBlock(t *testing.T, block string, env map[string]string, args ...string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("python3", append([]string{"-"}, args...)...)
	cmd.Stdin = strings.NewReader(block)
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	return cmd.CombinedOutput()
}

func TestShellAssetsParseAndProvisionReferencesExist(t *testing.T) {
	assets, err := filepath.Glob("*.sh")
	if err != nil || len(assets) == 0 {
		t.Fatalf("glob shell assets: %v", err)
	}
	for _, asset := range assets {
		if out, err := exec.Command("bash", "-n", asset).CombinedOutput(); err != nil {
			t.Fatalf("bash -n %s: %v\n%s", asset, err, out)
		}
	}
	provision := readAsset(t, "provision.sh")
	archiveAPI := readAsset(t, "archive-api.json.template")
	for _, required := range []string{
		"install-host.sh", "restore-discrawl.sh", "verify-deployment.sh", "config.toml.template",
		"archive-api.json.template", "discrawl-tail.service", "discrawl-sync.service", "discrawl-api.service",
		"backup-discrawl.service", "backup-discrawl.timer", "backup-lifecycle.json",
	} {
		if !strings.Contains(provision, required) {
			t.Errorf("provision does not reference %s", required)
		}
		if _, err := os.Stat(required); err != nil {
			t.Errorf("missing referenced asset %s: %v", required, err)
		}
	}
	if !strings.Contains(archiveAPI, `"allowed_source_cidr": "%%SERVERLESS_SUBNET_RANGE%%"`) ||
		!strings.Contains(provision, `s|%%SERVERLESS_SUBNET_RANGE%%|${SERVERLESS_SUBNET_RANGE}|g`) {
		t.Error("archive API must bind its fixed pre-auth limiter to the exact Direct VPC subnet")
	}
	for _, expected := range []string{
		"DISCRAWL_APIS_PRE_ENABLED", "gcloud services list --enabled", "required pre-enabled API is missing",
	} {
		if !strings.Contains(provision, expected) {
			t.Errorf("provision pre-enabled API contract missing %q", expected)
		}
	}
}

func TestHostRollbackAndBackupContracts(t *testing.T) {
	install := readAsset(t, "install-host.sh")
	restore := readAsset(t, "restore-discrawl.sh")
	tailUnit := readAsset(t, "discrawl-tail.service")
	syncUnit := readAsset(t, "discrawl-sync.service")
	apiUnit := readAsset(t, "discrawl-api.service")
	for name, unit := range map[string]string{"tail": tailUnit, "sync": syncUnit} {
		if !strings.Contains(unit, "Environment=DISCRAWL_DB_GROUP_READABLE=1") {
			t.Errorf("%s writer must preserve the deployment-only archive group-read grant", name)
		}
	}
	if !strings.Contains(apiUnit, "ReadOnlyPaths=/var/lib/discrawl/archive.db") {
		t.Error("archive API must remain kernel-enforced read-only even when the writer grants group read")
	}
	for _, expected := range []string{
		"apt-get install -y -qq ca-certificates curl gzip python3 sqlite3 systemd-resolved",
		"bot-token.env", "archive.db", "rollback_upgrade", "rm -f /var/lib/discrawl/projection/state.json",
		"discrawl-sync.service", "2 * live_bytes + wal_bytes + safety_bytes",
		"getent ahostsv4 metadata.google.internal 2>/dev/null", "awk 'NR == 1 {print $1}' || true",
		"systemctl restart discrawl-tail.service", "systemctl restart discrawl-api.service",
	} {
		if !strings.Contains(install, expected) {
			t.Errorf("installer missing rollback/capacity contract %q", expected)
		}
	}
	if !strings.Contains(install, "trap - ERR\n  set +e") || !strings.Contains(install, "stop_db_services\nif [[ ! -s") {
		t.Error("upgrade rollback must continue best-effort and main migration must require a successful service stop")
	}
	for name, contract := range map[string]struct {
		body string
		sink string
	}{
		"install-host.sh":     {install, "rm -f /var/lib/discrawl/archive.db /var/lib/discrawl/archive.db-wal /var/lib/discrawl/archive.db-shm"},
		"restore-discrawl.sh": {restore, "rm -f \"/var/lib/discrawl/${name}\""},
	} {
		for _, expected := range []string{
			"systemctl stop discrawl-sync.service discrawl-api.service discrawl-tail.service || return 1",
			"systemctl is-active --quiet \"${service}\"",
			"if ! stop_db_services >/dev/null 2>&1; then",
			"rollback refused to replace SQLite while a writer may still be active",
		} {
			if !strings.Contains(contract.body, expected) {
				t.Errorf("%s missing fail-closed writer contract %q", name, expected)
			}
		}
		gateAt := strings.Index(contract.body, "if ! stop_db_services >/dev/null 2>&1; then")
		sinkAt := strings.Index(contract.body, contract.sink)
		if gateAt < 0 || sinkAt < 0 || gateAt > sinkAt {
			t.Errorf("%s must prove all writers stopped before replacing SQLite", name)
		}
	}
	if strings.Contains(install, "runuser -u discrawl-tail") || strings.Contains(restore, "runuser -u discrawl-tail") {
		t.Error("installer and restore must execute Discord sync only through its metadata-denied unit")
	}
	backup := readAsset(t, "backup-discrawl.sh")
	for _, expected := range []string{
		"2 * live_bytes + wal_bytes + safety_bytes", "ifGenerationMatch=0", "computeMetadata/v1",
		"pragma quick_check", "daily/", "monthly/",
	} {
		if !strings.Contains(backup, expected) {
			t.Errorf("backup missing contract %q", expected)
		}
	}
	if strings.Contains(backup, "gcloud ") {
		t.Error("runtime backup must not depend on Cloud SDK")
	}
	trapAt := strings.Index(restore, "trap restore_old ERR")
	mainStopAt := -1
	if trapAt >= 0 {
		mainStopAt = strings.Index(restore[trapAt:], "stop_db_services\nrestore_started=")
	}
	if trapAt < 0 || mainStopAt < 0 {
		t.Error("restore rollback must be armed before stopping services")
	}
	for _, expected := range []string{"new_db_installed", "stamp >= started", "stop_db_services\nrestore_started="} {
		if !strings.Contains(restore, expected) {
			t.Errorf("restore missing partial-move/fresh-checkpoint contract %q", expected)
		}
	}
}

func TestIsolationAndRetentionContracts(t *testing.T) {
	provision := readAsset(t, "provision.sh")
	verify := readAsset(t, "verify-deployment.sh")
	for _, expected := range []string{
		"--size=30GB", "private-network-ip=${ARCHIVE_INTERNAL_IP},no-address", "--deletion-protection", "--vpc-egress", // vpc marker is checked below in product repos
		"roles/storage.objectCreator", "roles/secretmanager.secretAccessor", "e2-medium", "e2-micro",
	} {
		if expected == "--vpc-egress" {
			continue
		}
		if !strings.Contains(provision, expected) {
			t.Errorf("provision missing isolation contract %q", expected)
		}
	}
	for _, expected := range []string{
		`gcloud projects add-iam-policy-binding "${PROJECT_ID}" --member="serviceAccount:${archive_sa}" --role="${role}" \
    --condition=None`,
		`gcloud projects remove-iam-policy-binding "${PROJECT_ID}" --member="serviceAccount:${archive_sa}" \
  --role=roles/secretmanager.secretAccessor --condition=None`,
		`gcloud secrets remove-iam-policy-binding "${BOT_SECRET_ID}" --project="${PROJECT_ID}" \
  --member="serviceAccount:${archive_sa}" --role=roles/secretmanager.secretAccessor \
  --condition=None`,
		`gcloud storage buckets add-iam-policy-binding "gs://${BACKUP_BUCKET}" \
  --member="serviceAccount:${archive_sa}" --role=roles/storage.objectCreator \
  --condition=None`,
		`gcloud storage buckets remove-iam-policy-binding "gs://${BACKUP_BUCKET}" \
  --member="serviceAccount:${archive_sa}" --role=roles/storage.objectAdmin \
  --condition=None`,
	} {
		if !strings.Contains(provision, expected) {
			t.Errorf("provision IAM mutation must target the unconditional binding explicitly: %q", expected)
		}
	}
	for _, expected := range []string{
		"assert roles == required", "assert roles == set()", "BOT_SECRET_ID", "not nic.get(\"accessConfigs\")",
		"len(d[\"networkInterfaces\"]) == 1", "not d.get(\"tags\", {}).get(\"items\")", "endswith(\"/disks/\"+disk)",
		"target_sas and sa not in target_sas", "port_8787", "get-ancestors-iam-policy", "ANCESTOR_IAM",
		"protocol == \"all\"", "protocol not in (\"tcp\", \"6\")", "covers_port(spec, port)",
		"tombstoneSweepComplete", "attachmentUrlSweepComplete", `f["schemaVersion"]["integerValue"] == "2"`,
		"discrawl-tail-network-probe", "systemd-resolved.service",
		"metadata.google.internal", "/computeMetadata/v1/instance/id",
		"ProtectProc=invisible", "IPAddressDeny=169.254.169.254/32", "timedelta(minutes=20)", "stamp >= started", "http://127.0.0.1:8787/v1/status",
	} {
		if !strings.Contains(verify, expected) {
			t.Errorf("verify missing drift/readback contract %q", expected)
		}
	}
	bucketProjectAt := strings.Index(provision, "projectNumber")
	bucketMutationAt := strings.Index(provision, "gcloud storage buckets update")
	remoteInstallAt := strings.Index(provision, "gcloud compute scp")
	if bucketProjectAt < 0 || bucketMutationAt < 0 || remoteInstallAt < 0 ||
		bucketProjectAt > bucketMutationAt || bucketProjectAt > remoteInstallAt {
		t.Error("provision must prove backup bucket ownership before mutation or remote installation")
	}
	if !strings.Contains(provision, `gcloud storage buckets describe "gs://${BACKUP_BUCKET}" --raw --format=json`) {
		t.Error("backup bucket ownership proof must inspect the raw API schema containing projectNumber")
	}
	if !strings.Contains(provision, `gcloud storage buckets update "gs://${BACKUP_BUCKET}" --pap --versioning`) ||
		strings.Contains(provision, "--public-access-prevention=enforced") {
		t.Error("backup bucket hardening must use the current argument-free gcloud --pap flag")
	}
	if !strings.Contains(provision, "--weekly-schedule=sunday") || strings.Contains(provision, "--weekly-schedule=SUN") {
		t.Error("snapshot scheduling must use the current full gcloud weekday enum")
	}
	for _, expected := range []string{
		"gcloud compute instances delete-access-config", "public_access_rows", "not nic.get('ipv6AccessConfigs')",
	} {
		if !strings.Contains(provision, expected) {
			t.Errorf("provision must remove and reject every external VM access configuration: %q", expected)
		}
	}
	removeExternalAt := strings.Index(provision, "gcloud compute instances delete-access-config")
	convergenceAt := strings.Index(provision, `gcloud compute networks describe "${VPC_NETWORK}"`)
	if removeExternalAt < 0 || convergenceAt < 0 || removeExternalAt > convergenceAt {
		t.Error("existing VM public access must be removed before slower infrastructure convergence")
	}
	for _, fragileFormat := range []string{"sourceRanges.list():sort", "targetServiceAccounts.list():sort"} {
		if strings.Contains(provision, fragileFormat) {
			t.Errorf("firewall verification must use JSON rather than gcloud format expression %q", fragileFormat)
		}
	}
	for _, expected := range []string{
		`d.get("sourceRanges") == [source]`, `d.get("targetServiceAccounts") == [sa]`,
		`d.get("network", "").endswith("/"+network)`,
	} {
		if !strings.Contains(provision, expected) {
			t.Errorf("firewall JSON proof missing exact-boundary assertion %q", expected)
		}
	}
	var lifecycle struct {
		Rule []json.RawMessage `json:"rule"`
	}
	if err := json.Unmarshal([]byte(readAsset(t, "backup-lifecycle.json")), &lifecycle); err != nil {
		t.Fatal(err)
	}
	if len(lifecycle.Rule) != 3 {
		t.Fatalf("want 3 bounded lifecycle rules, got %d", len(lifecycle.Rule))
	}
}

func TestAggregateFirewallVerificationUnderstandsGCPPortEncodings(t *testing.T) {
	verify := readAsset(t, "verify-deployment.sh")
	block := pythonHeredocAfter(t, verify, `TARGET_FIREWALLS="${target_firewalls}" python3`)
	const (
		expectedName = "discrawl-archive-private"
		expectedCIDR = "10.20.0.0/26"
		network      = "discrawl-net"
		serviceAcct  = "discrawl@example.iam.gserviceaccount.com"
	)
	rule := func(name, source, protocol string, ports []string) map[string]any {
		allow := map[string]any{"IPProtocol": protocol}
		if ports != nil {
			allow["ports"] = ports
		}
		return map[string]any{
			"name": name, "network": "projects/example/global/networks/" + network,
			"direction": "INGRESS", "sourceRanges": []string{source},
			"targetServiceAccounts": []string{serviceAcct}, "allowed": []any{allow},
		}
	}
	run := func(rules []map[string]any) ([]byte, error) {
		t.Helper()
		body, err := json.Marshal(rules)
		if err != nil {
			t.Fatal(err)
		}
		return runPythonBlock(t, block, map[string]string{"TARGET_FIREWALLS": string(body)}, expectedName, expectedCIDR, network, serviceAcct)
	}
	expected := rule(expectedName, expectedCIDR, "tcp", []string{"8787"})
	if out, err := run([]map[string]any{expected}); err != nil {
		t.Fatalf("expected firewall rejected: %v\n%s", err, out)
	}
	udp := rule("irrelevant-udp", "0.0.0.0/0", "udp", []string{"8000-9000"})
	if out, err := run([]map[string]any{expected, udp}); err != nil {
		t.Fatalf("non-TCP rule should not affect port 8787 proof: %v\n%s", err, out)
	}
	for name, extra := range map[string]map[string]any{
		"tcp range":   rule("broad-range", "10.0.0.0/8", "tcp", []string{"8000-9000"}),
		"numeric TCP": rule("broad-numeric", "10.0.0.0/8", "6", []string{"8787"}),
		"all ports":   rule("broad-all", "10.0.0.0/8", "tcp", nil),
	} {
		if out, err := run([]map[string]any{expected, extra}); err == nil {
			t.Errorf("%s rule covering 8787 was not rejected: %s", name, out)
		}
	}
	malformed := rule("malformed", "10.0.0.0/8", "tcp", []string{"not-a-port"})
	if out, err := run([]map[string]any{expected, malformed}); err == nil {
		t.Errorf("malformed TCP port specification did not fail closed: %s", out)
	}
}

func TestProvisionRejectsForeignBackupBucketBeforeMutation(t *testing.T) {
	provision := readAsset(t, "provision.sh")
	block := pythonHeredocAfter(t, provision, `BUCKET_JSON="${bucket_json}" python3`)
	run := func(bucket map[string]any, region, project string) ([]byte, error) {
		t.Helper()
		body, err := json.Marshal(bucket)
		if err != nil {
			t.Fatal(err)
		}
		return runPythonBlock(t, block, map[string]string{"BUCKET_JSON": string(body)}, region, project)
	}
	local := map[string]any{"location": "US-CENTRAL1", "projectNumber": "123456789"}
	if out, err := run(local, "us-central1", "123456789"); err != nil {
		t.Fatalf("tenant-local backup bucket rejected: %v\n%s", err, out)
	}
	if out, err := run(local, "us-central1", "987654321"); err == nil {
		t.Errorf("foreign-project backup bucket was accepted: %s", out)
	}
	if out, err := run(local, "europe-west1", "123456789"); err == nil {
		t.Errorf("wrong-region backup bucket was accepted: %s", out)
	}
}

func TestAncestorIAMVerificationRejectsInheritedArchiveRoles(t *testing.T) {
	verify := readAsset(t, "verify-deployment.sh")
	block := pythonHeredocAfter(t, verify, `ANCESTOR_IAM="${ancestor_iam}" python3`)
	const member = "serviceAccount:discrawl@example.iam.gserviceaccount.com"
	required := []string{
		"roles/datastore.user", "roles/firebasedatabase.admin",
		"roles/logging.logWriter", "roles/monitoring.metricWriter",
	}
	bindings := make([]any, 0, len(required))
	for _, role := range required {
		bindings = append(bindings, map[string]any{"role": role, "members": []string{member}})
	}
	rows := []map[string]any{{"type": "project", "id": "example", "policy": map[string]any{"bindings": bindings}}}
	run := func(input []map[string]any) ([]byte, error) {
		t.Helper()
		body, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		return runPythonBlock(t, block, map[string]string{"ANCESTOR_IAM": string(body)}, member)
	}
	if out, err := run(rows); err != nil {
		t.Fatalf("exact ancestor IAM rejected: %v\n%s", err, out)
	}
	rowsWithUnrelated := append([]map[string]any{}, rows...)
	rowsWithUnrelated = append(rowsWithUnrelated, map[string]any{
		"type": "folder", "id": "123", "policy": map[string]any{"bindings": []any{
			map[string]any{"role": "roles/owner", "members": []string{"serviceAccount:other@example.iam.gserviceaccount.com"}},
		}},
	})
	if out, err := run(rowsWithUnrelated); err != nil {
		t.Fatalf("unrelated ancestor principal should be preserved: %v\n%s", err, out)
	}
	rowsWithExtra := append([]map[string]any{}, rows...)
	rowsWithExtra = append(rowsWithExtra, map[string]any{
		"type": "folder", "id": "123", "policy": map[string]any{"bindings": []any{
			map[string]any{"role": "roles/secretmanager.secretAccessor", "members": []string{member}},
		}},
	})
	if out, err := run(rowsWithExtra); err == nil {
		t.Errorf("inherited archive-SA role was not rejected: %s", out)
	}
}

func TestDiscordFacingUnitsCannotReachMetadataOrInspectProcesses(t *testing.T) {
	for _, name := range []string{"discrawl-tail.service", "discrawl-sync.service"} {
		unit := readAsset(t, name)
		for _, expected := range []string{
			"ProtectProc=invisible", "IPAddressDeny=169.254.169.254/32", "IPAddressDeny=::ffff:169.254.169.254/128", "IPAddressDeny=fd20:ce::254/128",
			"ExecStartPre=/usr/bin/test -x /usr/bin/curl", "ExecStartPre=/usr/bin/getent ahosts discord.com", "--noproxy \"*\"", "http://169.254.169.254/computeMetadata",
		} {
			if !strings.Contains(unit, expected) {
				t.Errorf("%s missing %q", name, expected)
			}
		}
	}
}

func TestProjectionCheckpointRejectsMergedFlagsFromOlderSchema(t *testing.T) {
	for _, name := range []string{"verify-deployment.sh", "restore-discrawl.sh"} {
		asset := readAsset(t, name)
		if !strings.Contains(asset, `f["schemaVersion"]["integerValue"] == "2"`) {
			t.Errorf("%s does not require exact projection schema v2", name)
		}
	}
}

func TestProvisionWaitsForIAPSSHAcrossHostRestarts(t *testing.T) {
	provision := readAsset(t, "provision.sh")
	for _, expected := range []string{
		"wait_for_iap_ssh()",
		`--tunnel-through-iap --quiet --command=true`,
		`echo "VM did not become reachable through IAP SSH"`,
	} {
		if !strings.Contains(provision, expected) {
			t.Errorf("provision missing restart readiness contract %q", expected)
		}
	}
	if strings.Count(provision, "wait_for_iap_ssh\n") != 2 {
		t.Fatalf("provision must wait before remote install and after steady-state resize")
	}
	if !strings.Contains(provision, "wait_for_iap_ssh\ngcloud compute scp") ||
		!strings.Contains(provision, "wait_for_iap_ssh\n\n\"${SCRIPT_DIR}/verify-deployment.sh\"") {
		t.Fatal("IAP SSH readiness waits must guard both remote installation and verification")
	}
}

func TestReleasePackagesBothBinaries(t *testing.T) {
	release := readAsset(t, filepath.Join("..", "..", ".goreleaser.yaml"))
	for _, expected := range []string{"id: discrawl", "id: discrawl-api", "main: ./cmd/discrawl-api"} {
		if !strings.Contains(release, expected) {
			t.Errorf("release missing %q", expected)
		}
	}
}
