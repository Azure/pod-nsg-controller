package main

import (
	"bytes"
	"os"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/config"
)

func TestParseOptionsVersionFlags(t *testing.T) {
	for _, arg := range []string{"-version", "--version"} {
		t.Run(arg, func(t *testing.T) {
			var output bytes.Buffer
			opts, err := parseOptions([]string{arg}, &output)
			if err != nil {
				t.Fatalf("parseOptions(%q): %v", arg, err)
			}
			if !opts.showVersion {
				t.Fatalf("parseOptions(%q) showVersion = false, want true", arg)
			}
			if output.Len() != 0 {
				t.Fatalf("parseOptions(%q) output = %q, want empty", arg, output.String())
			}
		})
	}
}

func TestParseOptionsControllerFlags(t *testing.T) {
	var output bytes.Buffer
	opts, err := parseOptions([]string{
		"--metrics-bind-address=:9090",
		"-health-probe-bind-address=:9091",
		"--leader-elect",
	}, &output)
	if err != nil {
		t.Fatalf("parseOptions: %v", err)
	}
	if opts.metricsAddr != ":9090" {
		t.Errorf("metricsAddr = %q, want :9090", opts.metricsAddr)
	}
	if opts.healthProbeAddr != ":9091" {
		t.Errorf("healthProbeAddr = %q, want :9091", opts.healthProbeAddr)
	}
	if !opts.enableLeaderElection {
		t.Error("enableLeaderElection = false, want true")
	}
	if opts.showVersion {
		t.Error("showVersion = true, want false")
	}
}

func TestWriteVersion(t *testing.T) {
	originalVersion := version
	version = "v1.2.3"
	t.Cleanup(func() { version = originalVersion })

	var output bytes.Buffer
	writeVersion(&output)
	if got, want := output.String(), "pod-nsg-controller v1.2.3\n"; got != want {
		t.Fatalf("writeVersion() = %q, want %q", got, want)
	}
}

func TestDefaultVersionIndicatesDevelopmentBuild(t *testing.T) {
	if defaultVersion != "development" {
		t.Fatalf("defaultVersion = %q, want development", defaultVersion)
	}
}

// ---------------------------------------------------------------------------
// Phase 6: armTuningReloader.reload() — mixed-success partial apply
// ---------------------------------------------------------------------------

func TestReloader_Reload_ValidRPS_InvalidConcurrency_AppliesRPSOnly(t *testing.T) {
	// Design: valid RPS + invalid max-parallel → only RPS updated, max-parallel
	// retains last-good value. reload() applies each field independently.
	dir := t.TempDir()
	t.Setenv("ARM_TUNING_DIR", dir)

	// Initial values.
	initial := config.ARMTuningConfig{
		ARMRateLimitRPS:      10,
		MaxConcurrentActions: 5,
	}

	log := zaptest.NewLogger(t)
	limiter := azure.NewARMRateLimiter(log, initial.ARMRateLimitRPS)
	executor := azure.NewExecutor(log, nil, initial.MaxConcurrentActions)

	reloader := newARMTuningReloader(log, 30*time.Second, initial, executor, limiter)

	// Write valid RPS, invalid concurrency.
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("50\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("garbage\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reloader.reload()

	// RPS should have been updated to 50 (valid field applied independently).
	if got := limiter.RPS(); got != 50 {
		t.Errorf("limiter.RPS() = %v, want 50 (valid field should be applied)", got)
	}

	// Max parallel should remain at 5 (invalid field keeps last-good).
	if got := executor.MaxParallel(); got != 5 {
		t.Errorf("executor.MaxParallel() = %v, want 5 (last-good retained)", got)
	}
}

func TestReloader_Reload_InvalidRPS_ValidConcurrency_AppliesConcurrencyOnly(t *testing.T) {
	// Design: invalid RPS + valid max-parallel → only max-parallel updated.
	dir := t.TempDir()
	t.Setenv("ARM_TUNING_DIR", dir)

	initial := config.ARMTuningConfig{
		ARMRateLimitRPS:      10,
		MaxConcurrentActions: 5,
	}

	log := zaptest.NewLogger(t)
	limiter := azure.NewARMRateLimiter(log, initial.ARMRateLimitRPS)
	executor := azure.NewExecutor(log, nil, initial.MaxConcurrentActions)

	reloader := newARMTuningReloader(log, 30*time.Second, initial, executor, limiter)

	// Write invalid RPS, valid concurrency.
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("not-a-number\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("20\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reloader.reload()

	// RPS should remain at 10 (invalid field keeps last-good).
	if got := limiter.RPS(); got != 10 {
		t.Errorf("limiter.RPS() = %v, want 10 (last-good retained)", got)
	}

	// Max parallel should have been updated to 20.
	if got := executor.MaxParallel(); got != 20 {
		t.Errorf("executor.MaxParallel() = %v, want 20 (valid field should be applied)", got)
	}
}

func TestReloader_Reload_BothFilesAbsent_KeepsCurrentValues(t *testing.T) {
	// Design: both files absent at runtime → non-fatal, keep r.current.
	dir := t.TempDir()
	t.Setenv("ARM_TUNING_DIR", dir)

	initial := config.ARMTuningConfig{
		ARMRateLimitRPS:      30,
		MaxConcurrentActions: 8,
	}

	log := zaptest.NewLogger(t)
	limiter := azure.NewARMRateLimiter(log, initial.ARMRateLimitRPS)
	executor := azure.NewExecutor(log, nil, initial.MaxConcurrentActions)

	reloader := newARMTuningReloader(log, 30*time.Second, initial, executor, limiter)

	// No files written — both absent.
	reloader.reload()

	// Values should remain unchanged.
	if got := limiter.RPS(); got != 30 {
		t.Errorf("limiter.RPS() = %v, want 30 (unchanged on absent files)", got)
	}
	if got := executor.MaxParallel(); got != 8 {
		t.Errorf("executor.MaxParallel() = %v, want 8 (unchanged on absent files)", got)
	}
}

func TestReloader_Reload_OneFileMissing_OneValid_AppliesValidField(t *testing.T) {
	// Design: one file missing + one valid → applies valid field, retains last-good
	// for the missing field.
	dir := t.TempDir()
	t.Setenv("ARM_TUNING_DIR", dir)

	initial := config.ARMTuningConfig{
		ARMRateLimitRPS:      10,
		MaxConcurrentActions: 5,
	}

	log := zaptest.NewLogger(t)
	limiter := azure.NewARMRateLimiter(log, initial.ARMRateLimitRPS)
	executor := azure.NewExecutor(log, nil, initial.MaxConcurrentActions)

	reloader := newARMTuningReloader(log, 30*time.Second, initial, executor, limiter)

	// Only write MAX_CONCURRENT_ACTIONS (RPS file missing).
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("15\n"), 0644); err != nil {
		t.Fatal(err)
	}

	reloader.reload()

	// RPS should remain at 10 (file missing → last-good).
	if got := limiter.RPS(); got != 10 {
		t.Errorf("limiter.RPS() = %v, want 10 (missing file retains last-good)", got)
	}

	// Max parallel should have been updated to 15.
	if got := executor.MaxParallel(); got != 15 {
		t.Errorf("executor.MaxParallel() = %v, want 15 (valid field applied)", got)
	}
}
