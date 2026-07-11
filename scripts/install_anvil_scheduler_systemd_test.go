package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runSchedulerInstallerDryRun executes the systemd installer in --dry-run
// --no-build mode (no root, no system mutation) with the given extra env and
// returns the combined stdout+stderr. Dry-run renders the env-file body it would
// write, prefixed with "  env| ", so the resident-polling knobs can be asserted
// without touching the host.
func runSchedulerInstallerDryRun(t *testing.T, extraEnv ...string) string {
	t.Helper()
	cmd := exec.Command("bash", "install-anvil-scheduler-systemd.sh", "--dry-run", "--no-build")
	cmd.Dir = scriptsDir(t)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("installer dry-run failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestSchedulerInstallerEnvFileBaseVars(t *testing.T) {
	out := runSchedulerInstallerDryRun(t)
	for _, want := range []string{
		"env| ANVIL_SCHEDULER_ADDR=127.0.0.1:3010",
		"env| ANVIL_SCHEDULER_STATE=/var/lib/anvil/scheduler.json",
		"env| ANVIL_SCHEDULER_QUOTA_STORE=/var/lib/anvil/tenants.json",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("env preview missing %q in:\n%s", want, out)
		}
	}
	// Optional knobs must be absent unless explicitly configured.
	for _, absent := range []string{
		"ANVIL_SCHEDULER_HOSTS_FILE=",
		"ANVIL_SCHEDULER_POLL_INTERVAL=",
		"ANVIL_SCHEDULER_API_TOKEN=",
	} {
		if strings.Contains(out, "env| "+absent) {
			t.Fatalf("env preview unexpectedly emitted %q in:\n%s", absent, out)
		}
	}
}

func TestSchedulerInstallerEnvFilePropagatesPollingKnobs(t *testing.T) {
	out := runSchedulerInstallerDryRun(t,
		"ANVIL_SCHEDULER_HOSTS_FILE=/etc/anvil/scheduler-hosts.json",
		"ANVIL_SCHEDULER_POLL_INTERVAL=5s",
		"ANVIL_SCHEDULER_RECONCILE_INTERVAL=10s",
		"ANVIL_SCHEDULER_HOST_TIMEOUT=2s",
		"ANVIL_SCHEDULER_FAILURE_THRESHOLD=4",
		"ANVIL_SCHEDULER_REQUIRE_PERSISTENCE=true",
	)
	for _, want := range []string{
		"env| ANVIL_SCHEDULER_HOSTS_FILE=/etc/anvil/scheduler-hosts.json",
		"env| ANVIL_SCHEDULER_POLL_INTERVAL=5s",
		"env| ANVIL_SCHEDULER_RECONCILE_INTERVAL=10s",
		"env| ANVIL_SCHEDULER_HOST_TIMEOUT=2s",
		"env| ANVIL_SCHEDULER_FAILURE_THRESHOLD=4",
		"env| ANVIL_SCHEDULER_REQUIRE_PERSISTENCE=true",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("env preview missing %q in:\n%s", want, out)
		}
	}
}

// The scheduler-to-daemon bearer token must be written to the env file (so
// resident polling can authenticate) but must never appear verbatim in the
// dry-run preview, matching the repo convention of keeping tokens out of logs.
func TestSchedulerInstallerRedactsAPITokenInPreview(t *testing.T) {
	out := runSchedulerInstallerDryRun(t, "ANVIL_SCHEDULER_API_TOKEN=super-secret-token")
	if strings.Contains(out, "super-secret-token") {
		t.Fatalf("dry-run preview leaked the API token:\n%s", out)
	}
	if !strings.Contains(out, "env| ANVIL_SCHEDULER_API_TOKEN=<redacted>") {
		t.Fatalf("env preview missing redacted API token line:\n%s", out)
	}
}

// A hosts-source file installs to the hosts-file path (declarative, reboot-safe
// inventory) and the installer defaults the destination when only the source is
// given.
func TestSchedulerInstallerHostsSourceInstall(t *testing.T) {
	out := runSchedulerInstallerDryRun(t, "ANVIL_SCHEDULER_HOSTS_SRC=/tmp/scheduler-hosts.json")
	if !strings.Contains(out, "/etc/anvil/scheduler-hosts.json") {
		t.Fatalf("expected default hosts-file destination in:\n%s", out)
	}
	if !strings.Contains(out, "env| ANVIL_SCHEDULER_HOSTS_FILE=/etc/anvil/scheduler-hosts.json") {
		t.Fatalf("hosts source did not wire ANVIL_SCHEDULER_HOSTS_FILE into env preview:\n%s", out)
	}
}
