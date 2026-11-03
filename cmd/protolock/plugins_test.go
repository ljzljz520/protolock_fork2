//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nilslice/protolock"
)

// writePluginScript creates an executable /bin/sh plugin in a temp dir and
// returns its absolute path.
func writePluginScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	if err != nil {
		t.Fatalf("write plugin script: %v", err)
	}
	return path
}

func testReport() *protolock.Report {
	return &protolock.Report{
		Warnings: []protolock.Warning{{
			Filepath: "builtin.proto",
			Message:  "builtin-rule-warning",
		}},
	}
}

func hasWarning(report *protolock.Report, message string) bool {
	for _, w := range report.Warnings {
		if w.Message == message {
			return true
		}
	}
	return false
}

// withShortTimeout temporarily overrides the plugin execution boundary.
func withShortTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := pluginTimeout
	pluginTimeout = d
	t.Cleanup(func() { pluginTimeout = old })
}

// withSmallOutputLimit temporarily overrides the plugin output boundary.
func withSmallOutputLimit(t *testing.T, n int64) {
	t.Helper()
	old := maxPluginOutput
	maxPluginOutput = n
	t.Cleanup(func() { maxPluginOutput = old })
}

// An existing JSON plugin succeeding must continue to work and contribute its
// warnings.
func TestRunPluginsNativeJSONSuccess(t *testing.T) {
	plugin := writePluginScript(t, "pp-ok",
		`printf '%s' '{"plugin_warnings":[{"filepath":"ok.proto","message":"healthy-plugin-warning"}]}'`,
	)

	report := testReport()
	report, err := runPlugins(plugin, report, false)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if !hasWarning(report, "healthy-plugin-warning") {
		t.Fatalf("plugin warning not collected: %+v", report.Warnings)
	}
	if !hasWarning(report, "builtin-rule-warning") {
		t.Fatalf("built-in warning was lost: %+v", report.Warnings)
	}
}

// A specified plugin that does not exist must fail the whole run, and every
// missing plugin must be reported (nothing swallowed).
func TestRunPluginsMissingExecutable(t *testing.T) {
	report := testReport()
	_, err := runPlugins("no-such-plugin-aaa,no-such-plugin-bbb", report, false)
	if err == nil {
		t.Fatal("expected error for missing plugins, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "executable file not found") {
		t.Fatalf("expected lookup error, got: %v", err)
	}
	if !strings.Contains(msg, "no-such-plugin-aaa") ||
		!strings.Contains(msg, "no-such-plugin-bbb") {
		t.Fatalf("expected both missing plugins reported, got: %v", err)
	}
}

// A missing .wasm file must fail instead of being silently skipped.
func TestRunPluginsMissingWasm(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.wasm")
	_, err := runPlugins(missing, testReport(), false)
	if err == nil || !strings.Contains(err.Error(), "wasm plugin not found") {
		t.Fatalf("expected wasm plugin not found error, got: %v", err)
	}
}

// A .wasm path pointing at garbage must surface the creation failure.
func TestRunPluginsInvalidWasm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.wasm")
	if err := os.WriteFile(path, []byte("definitely-not-wasm-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := runPlugins(path, testReport(), false)
	if err == nil || !strings.Contains(err.Error(), "failed to create wasm plugin") {
		t.Fatalf("expected wasm creation error, got: %v", err)
	}
}

// Output that cannot be parsed as JSON is a failure, not a success.
func TestRunPluginsUnparseableOutput(t *testing.T) {
	plugin := writePluginScript(t, "pp-badjson", `printf '%s' '<<<not-json>>>'`)
	_, err := runPlugins(plugin, testReport(), false)
	if err == nil || !strings.Contains(err.Error(), "could not be parsed as JSON") {
		t.Fatalf("expected JSON parse error, got: %v", err)
	}
}

// Empty output is unusable and must fail too.
func TestRunPluginsEmptyOutput(t *testing.T) {
	plugin := writePluginScript(t, "pp-empty", `printf ''`)
	_, err := runPlugins(plugin, testReport(), false)
	if err == nil || !strings.Contains(err.Error(), "returned no output") {
		t.Fatalf("expected no-output error, got: %v", err)
	}
}

// A plugin exiting non-zero fails the run, and its stderr diagnostics are
// included in the error rather than disappearing.
func TestRunPluginsNonZeroExit(t *testing.T) {
	plugin := writePluginScript(t, "pp-exit",
		`printf '%s' 'stderr-detail-xyz' >&2; exit 3`,
	)
	_, err := runPlugins(plugin, testReport(), false)
	if err == nil {
		t.Fatal("expected error for non-zero exit, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "plugin execution failed") {
		t.Fatalf("expected execution failure, got: %v", err)
	}
	if !strings.Contains(msg, "stderr-detail-xyz") {
		t.Fatalf("expected stderr output in error, got: %v", err)
	}
}

// A plugin returning plugin_error_message fails the run, but the warnings it
// returned alongside the error are still collected.
func TestRunPluginsPluginErrorMessage(t *testing.T) {
	plugin := writePluginScript(t, "pp-pluginerr",
		`printf '%s' '{"plugin_warnings":[{"filepath":"e.proto","message":"warning-from-erroring-plugin"}],"plugin_error_message":"plugin-error-xyz"}'`,
	)
	report, err := runPlugins(plugin, testReport(), false)
	if err == nil || !strings.Contains(err.Error(), "plugin-error-xyz") {
		t.Fatalf("expected plugin error message, got: %v", err)
	}
	if !hasWarning(report, "warning-from-erroring-plugin") {
		t.Fatalf("warnings from the erroring plugin were lost: %+v", report.Warnings)
	}
}

// When one plugin fails, warnings from healthy plugins and from built-in
// rules must survive, regardless of plugin ordering.
func TestRunPluginsWarningsNotLostOnFailure(t *testing.T) {
	good := writePluginScript(t, "pp-good",
		`printf '%s' '{"plugin_warnings":[{"filepath":"ok.proto","message":"healthy-plugin-warning"}]}'`,
	)
	bad := writePluginScript(t, "pp-bad", `printf '%s' 'garbage'`)

	for _, list := range []string{good + "," + bad, bad + "," + good} {
		report, err := runPlugins(list, testReport(), false)
		if err == nil {
			t.Fatalf("expected failure for plugin list %q, got nil", list)
		}
		if !hasWarning(report, "builtin-rule-warning") {
			t.Fatalf("built-in warning lost with list %q: %+v", list, report.Warnings)
		}
		if !hasWarning(report, "healthy-plugin-warning") {
			t.Fatalf("healthy plugin warning lost with list %q: %+v", list, report.Warnings)
		}
	}
}

// Output beyond the boundary is a failure instead of unbounded memory use.
func TestRunPluginsOutputLimit(t *testing.T) {
	withSmallOutputLimit(t, 1024)
	plugin := writePluginScript(t, "pp-big",
		`dd if=/dev/zero bs=1024 count=200 2>/dev/null | tr '\0' 'a'`,
	)
	_, err := runPlugins(plugin, testReport(), false)
	if err == nil || !strings.Contains(err.Error(), "exceeded the maximum allowed size") {
		t.Fatalf("expected output limit error, got: %v", err)
	}
}

// A hung plugin is terminated at the boundary: runPlugins returns a timeout
// error and neither the plugin process nor a spawned child process survive.
func TestRunPluginsTimeoutReleasesProcesses(t *testing.T) {
	withShortTimeout(t, 250*time.Millisecond)

	// child sleeps in the same process group; group kill must reap it too
	plugin := writePluginScript(t, "pp-hang",
		"sleep 271829 &\nwait $!",
	)

	start := time.Now()
	result := executePlugin(context.Background(), plugin, nil)
	if !errors.Is(result.err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got: %v", result.err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("timeout took too long: %v", d)
	}
	if result.pid == 0 {
		t.Fatal("expected plugin pid to be recorded")
	}

	// the direct child must be gone
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := syscall.Kill(result.pid, 0); errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out plugin process (pid %d) was not released", result.pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// the grandchild must also have been killed with the process group
	deadline = time.Now().Add(3 * time.Second)
	for {
		err := exec.Command("pgrep", "-f", "sleep 271829").Run()
		if err != nil {
			break // no matching process remains
		}
		if time.Now().After(deadline) {
			t.Fatal("child process of a timed-out plugin was left running")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The committed Extism sample plugin must keep working end to end.
func TestRunPluginsWASMSample(t *testing.T) {
	wasm, err := filepath.Abs(filepath.Join(
		"..", "..", "plugin-samples", "plugin-sample-wasm", "status.wasm",
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(wasm); statErr != nil {
		t.Skipf("sample wasm plugin not available: %v", statErr)
	}

	report, err := runPlugins(wasm, testReport(), false)
	if err != nil {
		t.Fatalf("expected wasm plugin success, got: %v", err)
	}
	if !hasWarning(report, "An Extism plugin ran and checked the status of the proto.lock files") {
		t.Fatalf("wasm plugin warning not collected: %+v", report.Warnings)
	}
}
