package main

import (
	"os"
	"testing"

	"go.uber.org/zap/zaptest"

	"github.com/Azure/pod-nsg-controller/internal/azure"
	"github.com/Azure/pod-nsg-controller/internal/config"
)

// ---------------------------------------------------------------------------
// Phase 6: armTuningReloader.reload() — mixed-success partial apply
// ---------------------------------------------------------------------------

func TestReloader_Reload_ValidRPS_InvalidConcurrency_AppliesRPSOnly(t *testing.T) {
	// Design: valid RPS + invalid max-parallel → only RPS updated, max-parallel
	// retains last-good value. Current implementation returns early on any error.
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

	reloader := newARMTuningReloader(log, 30_000_000_000, initial, executor, limiter)

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

	reloader := newARMTuningReloader(log, 30_000_000_000, initial, executor, limiter)

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

	reloader := newARMTuningReloader(log, 30_000_000_000, initial, executor, limiter)

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

	reloader := newARMTuningReloader(log, 30_000_000_000, initial, executor, limiter)

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
