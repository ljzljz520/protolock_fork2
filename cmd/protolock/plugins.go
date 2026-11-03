package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"
	"github.com/nilslice/protolock"
	"github.com/nilslice/protolock/extend"
)

const logPrefix = "[protolock]"

// Execution and output boundaries applied uniformly to native and WASM
// plugins. They are variables (not constants) so that tests can use shorter
// values; production callers always use the defaults below.
var (
	// pluginTimeout bounds how long any single plugin is allowed to run. A
	// plugin that does not finish in time is terminated and reported as a
	// failure, so one hung plugin can never block a status/commit indefinitely.
	pluginTimeout = 30 * time.Second

	// maxPluginOutput bounds the amount of output accepted from a plugin.
	// Plugins producing more are reported as failures instead of having
	// protolock allocate unbounded memory for their output.
	maxPluginOutput int64 = 10 << 20 // 10 MiB

	// maxErrorOutput caps how much captured plugin output is embedded in an
	// accumulated error message. The full bounded output may still be in
	// memory, but error messages stay readable.
	maxErrorOutput = 4 << 10 // 4 KiB
)

// pluginResult is the final outcome of running exactly one plugin. Every
// listed plugin produces one, regardless of how it terminated: results for
// failing plugins never disappear.
type pluginResult struct {
	name string
	path string
	// pid is the native plugin's OS process id (0 for WASM) and allows
	// verification that timed-out processes are released.
	pid       int
	warnings  []protolock.Warning
	rawOutput []byte
	err       error
}

func runPlugins(
	pluginList string,
	report *protolock.Report,
	debug bool,
) (*protolock.Report, error) {
	if report == nil {
		report = &protolock.Report{}
	}

	names := strings.Split(pluginList, ",")
	plugins := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			plugins = append(plugins, name)
		}
	}
	if len(plugins) == 0 {
		return report, nil
	}

	inputData := &bytes.Buffer{}
	err := json.NewEncoder(inputData).Encode(&extend.Data{
		Current:           report.Current,
		Updated:           report.Updated,
		ProtolockWarnings: report.Warnings,
		PluginWarnings:    []protolock.Warning{},
	})
	if err != nil {
		return report, err
	}
	input := inputData.Bytes()

	// Each plugin writes only to its own pre-allocated result slot, and the
	// main goroutine reads them only after the wait group completes, so the
	// aggregation needs no locks or channels.
	results := make([]pluginResult, len(plugins))

	wg := &sync.WaitGroup{}
	for i, name := range plugins {
		wg.Add(1)

		// run all provided plugins in parallel, each receiving the same
		// current and updated Protolock structs from the `protolock status`
		// call
		go func(i int, name string) {
			defer wg.Done()
			if debug {
				fmt.Fprintln(os.Stderr, logPrefix, name, "running plugin")
			}
			results[i] = executePlugin(context.Background(), name, input)
		}(i, name)
	}
	wg.Wait()

	// Collect every plugin's final result: warnings from successful plugins
	// are kept even when other plugins failed, so neither built-in warnings
	// nor warnings from healthy plugins are ever lost.
	var pluginErrors []string
	for _, result := range results {
		report.Warnings = append(report.Warnings, result.warnings...)
		if result.err != nil {
			pluginErrors = append(pluginErrors, wrapPluginErr(
				result.name, result.path, result.err, result.rawOutput,
			).Error())
		}
	}

	if len(pluginErrors) > 0 {
		return report, fmt.Errorf(
			"accumulated plugin errors: \n%s",
			strings.Join(pluginErrors, "\n"),
		)
	}

	return report, nil
}

// executePlugin runs one plugin under a shared execution boundary: both
// native binaries and WASM modules get the same timeout semantics and must
// either return parseable JSON results or be reported as failures.
func executePlugin(parent context.Context, name string, input []byte) pluginResult {
	ctx, cancel := context.WithTimeout(parent, pluginTimeout)
	defer cancel()

	if strings.HasSuffix(name, ".wasm") {
		return runWASMPlugin(ctx, name, input)
	}
	return runNativePlugin(ctx, name, input)
}

// runNativePlugin executes a plugin executable located either on the user's
// OS executable path (as reported by exec.LookPath) or at a direct path.
func runNativePlugin(ctx context.Context, name string, input []byte) pluginResult {
	result := pluginResult{name: name}

	path, err := exec.LookPath(name)
	if err != nil {
		result.path = name
		result.err = fmt.Errorf("plugin executable not found: %w", err)
		return result
	}
	result.path = path

	cmd := exec.CommandContext(ctx, path)
	cmd.Stdin = bytes.NewReader(input)

	// stdout carries the JSON result and is bounded; stderr is captured
	// separately so diagnostics never corrupt the JSON decoding.
	stdout := &limitedBuffer{limit: maxPluginOutput}
	stderr := &limitedBuffer{limit: maxPluginOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// ensure a timeout kills the whole process group (and child processes),
	// and reaps I/O held open by orphaned children
	configurePluginProcess(cmd)

	if err := cmd.Start(); err != nil {
		result.err = fmt.Errorf("failed to start plugin: %w", err)
		return result
	}
	result.pid = cmd.Process.Pid

	waitErr := cmd.Wait()

	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.rawOutput = stderr.Bytes()
		result.err = fmt.Errorf(
			"plugin timed out after %s: %w", pluginTimeout, context.DeadlineExceeded,
		)
		return result
	case waitErr != nil:
		result.rawOutput = stderr.Bytes()
		result.err = fmt.Errorf("plugin execution failed: %w", waitErr)
		return result
	}

	if stdout.truncated {
		result.rawOutput = stdout.Bytes()
		result.err = fmt.Errorf(
			"plugin output exceeded the maximum allowed size of %d bytes",
			maxPluginOutput,
		)
		return result
	}

	applyPluginOutput(&result, stdout.Bytes())
	return result
}

// runWASMPlugin executes an Extism (WASI) plugin exporting a "status"
// function with the same input/output JSON contract as native plugins.
func runWASMPlugin(ctx context.Context, name string, input []byte) pluginResult {
	result := pluginResult{name: name, path: name}

	if _, err := os.Stat(name); err != nil {
		result.err = fmt.Errorf("wasm plugin not found: %w", err)
		return result
	}

	manifest := extism.Manifest{
		Wasm: []extism.Wasm{extism.WasmFile{Path: name}},
		// A non-zero Timeout (in milliseconds) both bounds the call and
		// enables the wazero runtime's close-on-context-done behavior, so a
		// stuck WASM call is interrupted when the deadline passes.
		Timeout: uint64(pluginTimeout / time.Millisecond),
		// TODO: consider enabling external configuration to add hosts and paths
		// AllowedHosts: []string{},
		// AllowedPaths: map[string]string{},
	}

	// NewPlugin receives the deadline context directly so wazero interrupts
	// a stuck call when the boundary expires (WithCloseOnContextDone is
	// enabled because manifest.Timeout is non-zero).
	plugin, err := extism.NewPlugin(ctx, manifest, extism.PluginConfig{EnableWasi: true}, nil)
	if err != nil {
		result.err = fmt.Errorf("failed to create wasm plugin: %w", err)
		return result
	}
	// release the wazero runtime/compiled module in every outcome, including
	// timeouts and failed calls; the engine is closed even if ctx has expired
	defer func() { _ = plugin.Close() }()

	exitCode, output, callErr := plugin.Call("status", input)

	// a call interrupted by its deadline fails; surface the uniform timeout
	// result whenever our outer boundary has expired too
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		result.err = fmt.Errorf(
			"plugin timed out after %s: %w", pluginTimeout, context.DeadlineExceeded,
		)
		return result
	case callErr != nil:
		result.rawOutput = output
		result.err = fmt.Errorf(
			"wasm plugin execution failed (exit code %d): %w", exitCode, callErr,
		)
		return result
	}

	if int64(len(output)) > maxPluginOutput {
		result.err = fmt.Errorf(
			"plugin output exceeded the maximum allowed size of %d bytes",
			maxPluginOutput,
		)
		return result
	}

	applyPluginOutput(&result, output)
	return result
}

// applyPluginOutput decodes the shared JSON contract and records the
// plugin's final result. Unparseable or empty output and a non-empty
// plugin_error_message are failures, never silent successes.
func applyPluginOutput(result *pluginResult, output []byte) {
	if len(bytes.TrimSpace(output)) == 0 {
		result.err = errors.New("plugin returned no output")
		return
	}

	pluginData := &extend.Data{}
	if err := json.Unmarshal(output, pluginData); err != nil {
		result.rawOutput = output
		result.err = fmt.Errorf("plugin output could not be parsed as JSON: %w", err)
		return
	}

	result.warnings = pluginData.PluginWarnings
	if pluginData.PluginErrorMessage != "" {
		// the message is the error itself; the echoed JSON input adds no
		// diagnostic value and can be very large, so it is not attached.
		result.err = errors.New(pluginData.PluginErrorMessage)
	}
}

// limitedBuffer is an io.Writer that captures up to limit bytes and drains
// anything beyond it (recording truncation) so a chatty plugin cannot block
// or cause unbounded memory growth.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - int64(b.buf.Len())
	switch {
	case remaining <= 0:
		// drain without storing so the plugin never blocks on a full pipe
		b.truncated = true
		return len(p), nil
	case int64(len(p)) <= remaining:
		_, _ = b.buf.Write(p)
		return len(p), nil
	default:
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		// report the whole slice as consumed to drain the remainder
		return len(p), nil
	}
}

func (b *limitedBuffer) Bytes() []byte {
	return b.buf.Bytes()
}

func wrapPluginErr(name, path string, err error, output []byte) error {
	return fmt.Errorf(
		"%s (%s): %v\n%s",
		name, path, err, strings.ReplaceAll(
			string(capOutput(output)),
			protolock.ProtoSep, protolock.FileSep,
		),
	)
}

// capOutput bounds the plugin output embedded in error messages.
func capOutput(output []byte) []byte {
	if len(output) <= maxErrorOutput {
		return output
	}
	capped := make([]byte, 0, maxErrorOutput+len("\n...[truncated]"))
	capped = append(capped, output[:maxErrorOutput]...)
	return append(capped, []byte("\n...[truncated]")...)
}
