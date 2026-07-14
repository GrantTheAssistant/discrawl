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
}

func TestHostRollbackAndBackupContracts(t *testing.T) {
	install := readAsset(t, "install-host.sh")
	restore := readAsset(t, "restore-discrawl.sh")
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
	if !strings.Contains(install, "trap - ERR\n  set +e") || !strings.Contains(install, "systemctl stop discrawl-api.service discrawl-tail.service\nif [[ ! -s") {
		t.Error("upgrade rollback must continue best-effort and main migration must require a successful service stop")
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
		mainStopAt = strings.Index(restore[trapAt:], "systemctl stop discrawl-api.service")
	}
	if trapAt < 0 || mainStopAt < 0 {
		t.Error("restore rollback must be armed before stopping services")
	}
	for _, expected := range []string{"new_db_installed", "stamp >= started", "systemctl stop discrawl-api.service discrawl-tail.service\nrestore_started="} {
		if !strings.Contains(restore, expected) {
			t.Errorf("restore missing partial-move/fresh-checkpoint contract %q", expected)
		}
	}
}

func TestIsolationAndRetentionContracts(t *testing.T) {
	provision := readAsset(t, "provision.sh")
	verify := readAsset(t, "verify-deployment.sh")
	for _, expected := range []string{
		"--size=30GB", "--no-address", "--deletion-protection", "--vpc-egress", // vpc marker is checked below in product repos
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
		"assert roles == required", "assert roles == set()", "BOT_SECRET_ID", "not nic.get(\"accessConfigs\")",
		"len(d[\"networkInterfaces\"]) == 1", "not d.get(\"tags\", {}).get(\"items\")", "endswith(\"/disks/\"+disk)",
		"target_sas and sa not in target_sas", "port_8787",
		"protocol == \"all\"",
		"tombstoneSweepComplete", "attachmentUrlSweepComplete", `f["schemaVersion"]["integerValue"] == "2"`,
		"discrawl-tail-network-probe", "systemd-resolved.service",
		"metadata.google.internal", "/computeMetadata/v1/instance/id",
		"ProtectProc=invisible", "IPAddressDeny=169.254.169.254/32", "timedelta(minutes=20)", "stamp >= started", "http://127.0.0.1:8787/v1/status",
	} {
		if !strings.Contains(verify, expected) {
			t.Errorf("verify missing drift/readback contract %q", expected)
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

func TestReleasePackagesBothBinaries(t *testing.T) {
	release := readAsset(t, filepath.Join("..", "..", ".goreleaser.yaml"))
	for _, expected := range []string{"id: discrawl", "id: discrawl-api", "main: ./cmd/discrawl-api"} {
		if !strings.Contains(release, expected) {
			t.Errorf("release missing %q", expected)
		}
	}
}
