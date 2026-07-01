package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGenerateSQLCAnchorsToRepoRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("generate-sqlc.sh is a POSIX shell script")
	}

	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash is not available")
	}

	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate test file")
	}
	scriptDir := filepath.Dir(testFile)
	repoRoot := filepath.Dir(scriptDir)

	tempDir := t.TempDir()
	recordPwd := filepath.Join(tempDir, "pwd")
	recordArgs := filepath.Join(tempDir, "args")
	stubGo := filepath.Join(tempDir, "go")
	stub := "#!/usr/bin/env bash\nprintf '%s\\n' \"$PWD\" > \"$RECORD_PWD\"\nprintf '%s\\n' \"$*\" > \"$RECORD_ARGS\"\n"
	if err := os.WriteFile(stubGo, []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), bash, "./generate-sqlc.sh")
	cmd.Dir = scriptDir
	cmd.Env = append(
		os.Environ(),
		"PATH="+tempDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RECORD_PWD="+recordPwd,
		"RECORD_ARGS="+recordArgs,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generate-sqlc.sh failed: %v\n%s", err, output)
	}

	gotPwdBytes, err := os.ReadFile(recordPwd)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(gotPwdBytes)); got != repoRoot {
		t.Fatalf("go ran from %q, want %q", got, repoRoot)
	}

	gotArgsBytes, err := os.ReadFile(recordArgs)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate"
	if got := strings.TrimSpace(string(gotArgsBytes)); got != wantArgs {
		t.Fatalf("go args = %q, want %q", got, wantArgs)
	}
}
