package config

import (
	"errors"
	"os"
	"testing"
)

// ---------- ARMRateLimitRPS ----------

func TestLoad_ARMRateLimitRPS_Default20(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	// Unset to get default.
	os.Unsetenv("ARM_RATE_LIMIT_RPS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Phase 6: default moved from 10 to 20.
	if cfg.ARMRateLimitRPS != 20.0 {
		t.Errorf("expected default ARMRateLimitRPS=20, got %v", cfg.ARMRateLimitRPS)
	}
}

func TestLoad_ARMRateLimitRPS_Custom(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("ARM_RATE_LIMIT_RPS", "25.5")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.ARMRateLimitRPS != 25.5 {
		t.Errorf("expected ARMRateLimitRPS=25.5, got %v", cfg.ARMRateLimitRPS)
	}
}

func TestLoad_ARMRateLimitRPS_InvalidFails(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("ARM_RATE_LIMIT_RPS", "not-a-number")

	_, err := Load()
	if err == nil {
		t.Error("expected error for invalid ARM_RATE_LIMIT_RPS, got nil")
	}
}

func TestLoad_ARMRateLimitRPS_ZeroFails(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("ARM_RATE_LIMIT_RPS", "0")

	_, err := Load()
	if err == nil {
		t.Error("expected error for ARM_RATE_LIMIT_RPS=0 (must be > 0), got nil")
	}
}

func TestLoad_ARMRateLimitRPS_NegativeFails(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("ARM_RATE_LIMIT_RPS", "-5")

	_, err := Load()
	if err == nil {
		t.Error("expected error for negative ARM_RATE_LIMIT_RPS, got nil")
	}
}

// ---------- MaxConcurrentActions ----------

func TestLoad_MaxConcurrentActions_Default10(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	os.Unsetenv("MAX_CONCURRENT_ACTIONS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Phase 6: default moved from 5 to 10.
	if cfg.MaxConcurrentActions != 10 {
		t.Errorf("expected default MaxConcurrentActions=10, got %d", cfg.MaxConcurrentActions)
	}
}

func TestLoad_MaxConcurrentActions_Custom(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("MAX_CONCURRENT_ACTIONS", "20")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.MaxConcurrentActions != 20 {
		t.Errorf("expected MaxConcurrentActions=20, got %d", cfg.MaxConcurrentActions)
	}
}

func TestLoad_MaxConcurrentActions_InvalidFails(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("MAX_CONCURRENT_ACTIONS", "abc")

	_, err := Load()
	if err == nil {
		t.Error("expected error for invalid MAX_CONCURRENT_ACTIONS, got nil")
	}
}

func TestLoad_MaxConcurrentActions_ZeroFails(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("MAX_CONCURRENT_ACTIONS", "0")

	_, err := Load()
	if err == nil {
		t.Error("expected error for MAX_CONCURRENT_ACTIONS=0 (must be >= 1), got nil")
	}
}

func TestLoad_MaxConcurrentActions_NegativeFails(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("MAX_CONCURRENT_ACTIONS", "-1")

	_, err := Load()
	if err == nil {
		t.Error("expected error for negative MAX_CONCURRENT_ACTIONS, got nil")
	}
}

// ---------------------------------------------------------------------------
// Phase 6: Tunable ARM Concurrency — new default validation
// ---------------------------------------------------------------------------

func TestLoad_ARMRateLimitRPS_Phase6Default20(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	os.Unsetenv("ARM_RATE_LIMIT_RPS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Phase 6 moves default from 10 to 20.
	if cfg.ARMRateLimitRPS != 20.0 {
		t.Errorf("Phase 6: expected default ARMRateLimitRPS=20, got %v", cfg.ARMRateLimitRPS)
	}
}

func TestLoad_MaxConcurrentActions_Phase6Default10(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	os.Unsetenv("MAX_CONCURRENT_ACTIONS")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Phase 6 moves default from 5 to 10.
	if cfg.MaxConcurrentActions != 10 {
		t.Errorf("Phase 6: expected default MaxConcurrentActions=10, got %d", cfg.MaxConcurrentActions)
	}
}

// ---------------------------------------------------------------------------
// Phase 6: LoadARMTuningConfig — dedicated loader
// ---------------------------------------------------------------------------

func TestLoadARMTuningConfig_Defaults(t *testing.T) {
	// No tuning files, no env → Phase 6 defaults.
	os.Unsetenv("ARM_RATE_LIMIT_RPS")
	os.Unsetenv("MAX_CONCURRENT_ACTIONS")

	tuning, err := LoadARMTuningConfig()
	if err != nil {
		t.Fatalf("LoadARMTuningConfig() error: %v", err)
	}
	if tuning.ARMRateLimitRPS != 20.0 {
		t.Errorf("expected ARMRateLimitRPS=20, got %v", tuning.ARMRateLimitRPS)
	}
	if tuning.MaxConcurrentActions != 10 {
		t.Errorf("expected MaxConcurrentActions=10, got %d", tuning.MaxConcurrentActions)
	}
}

func TestLoadARMTuningConfig_FromFiles(t *testing.T) {
	// Create temp tuning files.
	dir := t.TempDir()
	rpsFile := dir + "/ARM_RATE_LIMIT_RPS"
	concFile := dir + "/MAX_CONCURRENT_ACTIONS"

	if err := os.WriteFile(rpsFile, []byte("30.5\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concFile, []byte("15\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARM_TUNING_DIR", dir)

	tuning, err := LoadARMTuningConfig()
	if err != nil {
		t.Fatalf("LoadARMTuningConfig() error: %v", err)
	}
	if tuning.ARMRateLimitRPS != 30.5 {
		t.Errorf("expected ARMRateLimitRPS=30.5, got %v", tuning.ARMRateLimitRPS)
	}
	if tuning.MaxConcurrentActions != 15 {
		t.Errorf("expected MaxConcurrentActions=15, got %d", tuning.MaxConcurrentActions)
	}
}

func TestLoadARMTuningConfig_InvalidFileValuesFail(t *testing.T) {
	dir := t.TempDir()
	rpsFile := dir + "/ARM_RATE_LIMIT_RPS"
	concFile := dir + "/MAX_CONCURRENT_ACTIONS"

	// Invalid: non-numeric RPS
	if err := os.WriteFile(rpsFile, []byte("not-a-number\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concFile, []byte("10\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARM_TUNING_DIR", dir)

	_, err := LoadARMTuningConfig()
	if err == nil {
		t.Error("expected error for invalid file-based ARM_RATE_LIMIT_RPS, got nil")
	}
}

func TestLoadARMTuningConfig_ZeroRPSFileFails(t *testing.T) {
	dir := t.TempDir()
	rpsFile := dir + "/ARM_RATE_LIMIT_RPS"
	concFile := dir + "/MAX_CONCURRENT_ACTIONS"

	if err := os.WriteFile(rpsFile, []byte("0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concFile, []byte("10\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARM_TUNING_DIR", dir)

	_, err := LoadARMTuningConfig()
	if err == nil {
		t.Error("expected error for ARM_RATE_LIMIT_RPS=0 from file, got nil")
	}
}

func TestLoadARMTuningConfig_ZeroConcurrencyFileFails(t *testing.T) {
	dir := t.TempDir()
	rpsFile := dir + "/ARM_RATE_LIMIT_RPS"
	concFile := dir + "/MAX_CONCURRENT_ACTIONS"

	if err := os.WriteFile(rpsFile, []byte("20\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concFile, []byte("0\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARM_TUNING_DIR", dir)

	_, err := LoadARMTuningConfig()
	if err == nil {
		t.Error("expected error for MAX_CONCURRENT_ACTIONS=0 from file, got nil")
	}
}

func TestLoadARMTuningConfig_FileTakesPrecedenceOverEnv(t *testing.T) {
	dir := t.TempDir()
	rpsFile := dir + "/ARM_RATE_LIMIT_RPS"
	concFile := dir + "/MAX_CONCURRENT_ACTIONS"

	if err := os.WriteFile(rpsFile, []byte("35\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(concFile, []byte("12\n"), 0644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARM_TUNING_DIR", dir)
	t.Setenv("ARM_RATE_LIMIT_RPS", "99")
	t.Setenv("MAX_CONCURRENT_ACTIONS", "99")

	tuning, err := LoadARMTuningConfig()
	if err != nil {
		t.Fatalf("LoadARMTuningConfig() error: %v", err)
	}
	// File-based values should take precedence over env.
	if tuning.ARMRateLimitRPS != 35 {
		t.Errorf("expected file-based ARMRateLimitRPS=35 over env=99, got %v", tuning.ARMRateLimitRPS)
	}
	if tuning.MaxConcurrentActions != 12 {
		t.Errorf("expected file-based MaxConcurrentActions=12 over env=99, got %d", tuning.MaxConcurrentActions)
	}
}

// ---------------------------------------------------------------------------
// Phase 6: ErrTuningFilesAbsent — startup fallback path
// ---------------------------------------------------------------------------

func TestLoadARMTuningConfig_FilesAbsent_ReturnsErrTuningFilesAbsent(t *testing.T) {
	// Point ARM_TUNING_DIR at an empty directory (files not mounted yet).
	dir := t.TempDir()
	t.Setenv("ARM_TUNING_DIR", dir)
	os.Unsetenv("ARM_RATE_LIMIT_RPS")
	os.Unsetenv("MAX_CONCURRENT_ACTIONS")

	_, err := LoadARMTuningConfig()
	if err == nil {
		t.Fatal("expected error when tuning files are absent, got nil")
	}
	if !errors.Is(err, ErrTuningFilesAbsent) {
		t.Errorf("expected ErrTuningFilesAbsent, got: %v", err)
	}
}

func TestLoadARMTuningFromEnv_FallbackDefaults(t *testing.T) {
	// Simulate the startup fallback: no env set, should return Phase 6 defaults.
	os.Unsetenv("ARM_RATE_LIMIT_RPS")
	os.Unsetenv("MAX_CONCURRENT_ACTIONS")

	tuning, err := LoadARMTuningFromEnv()
	if err != nil {
		t.Fatalf("LoadARMTuningFromEnv() error: %v", err)
	}
	if tuning.ARMRateLimitRPS != 20.0 {
		t.Errorf("expected default ARMRateLimitRPS=20, got %v", tuning.ARMRateLimitRPS)
	}
	if tuning.MaxConcurrentActions != 10 {
		t.Errorf("expected default MaxConcurrentActions=10, got %d", tuning.MaxConcurrentActions)
	}
}

func TestLoadARMTuningFromEnv_RespectsEnvOverrides(t *testing.T) {
	t.Setenv("ARM_RATE_LIMIT_RPS", "42.5")
	t.Setenv("MAX_CONCURRENT_ACTIONS", "7")

	tuning, err := LoadARMTuningFromEnv()
	if err != nil {
		t.Fatalf("LoadARMTuningFromEnv() error: %v", err)
	}
	if tuning.ARMRateLimitRPS != 42.5 {
		t.Errorf("expected ARMRateLimitRPS=42.5, got %v", tuning.ARMRateLimitRPS)
	}
	if tuning.MaxConcurrentActions != 7 {
		t.Errorf("expected MaxConcurrentActions=7, got %d", tuning.MaxConcurrentActions)
	}
}

func TestLoadARMTuningConfig_FilesAbsent_ThenEnvFallbackWorks(t *testing.T) {
	// End-to-end startup fallback: ARM_TUNING_DIR set but files absent →
	// ErrTuningFilesAbsent, then LoadARMTuningFromEnv succeeds with env values.
	dir := t.TempDir()
	t.Setenv("ARM_TUNING_DIR", dir)
	t.Setenv("ARM_RATE_LIMIT_RPS", "25")
	t.Setenv("MAX_CONCURRENT_ACTIONS", "8")

	_, err := LoadARMTuningConfig()
	if !errors.Is(err, ErrTuningFilesAbsent) {
		t.Fatalf("expected ErrTuningFilesAbsent, got: %v", err)
	}

	// Caller (cmd/main.go) falls back here:
	tuning, err := LoadARMTuningFromEnv()
	if err != nil {
		t.Fatalf("LoadARMTuningFromEnv() error: %v", err)
	}
	if tuning.ARMRateLimitRPS != 25 {
		t.Errorf("expected ARMRateLimitRPS=25, got %v", tuning.ARMRateLimitRPS)
	}
	if tuning.MaxConcurrentActions != 8 {
		t.Errorf("expected MaxConcurrentActions=8, got %d", tuning.MaxConcurrentActions)
	}
}

// ---------------------------------------------------------------------------
// Phase 6: Runtime reload — last-good retention pattern
// ---------------------------------------------------------------------------

func TestLoadARMTuningConfig_RuntimeReload_LastGoodRetention(t *testing.T) {
	// Simulates what armTuningReloader.reload() does:
	// 1. Load good values from files
	// 2. Files become invalid → LoadARMTuningConfig returns error
	// 3. Caller (reloader) keeps last-good values
	dir := t.TempDir()
	t.Setenv("ARM_TUNING_DIR", dir)

	// Step 1: Write valid tuning files.
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("30\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("12\n"), 0644); err != nil {
		t.Fatal(err)
	}

	lastGood, err := LoadARMTuningConfig()
	if err != nil {
		t.Fatalf("initial load should succeed: %v", err)
	}
	if lastGood.ARMRateLimitRPS != 30 || lastGood.MaxConcurrentActions != 12 {
		t.Fatalf("unexpected initial values: %+v", lastGood)
	}

	// Step 2: Corrupt the files (simulate invalid runtime snapshot).
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("garbage\n"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err = LoadARMTuningConfig()
	if err == nil {
		t.Fatal("expected error for corrupted tuning file, got nil")
	}

	// Step 3: The reloader pattern — on error, keep last-good.
	// (This verifies the contract that LoadARMTuningConfig errors out
	// rather than silently returning bad values, enabling the reloader
	// to preserve r.current.)
	if lastGood.ARMRateLimitRPS != 30 || lastGood.MaxConcurrentActions != 12 {
		t.Errorf("last-good values should be preserved: %+v", lastGood)
	}
}

// ---------------------------------------------------------------------------
// Phase 6: NaN/Inf validation — non-finite values must be rejected
// ---------------------------------------------------------------------------

func TestLoad_ARMRateLimitRPS_NaN_Fails(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("ARM_RATE_LIMIT_RPS", "NaN")

	_, err := Load()
	if err == nil {
		t.Error("expected error for ARM_RATE_LIMIT_RPS=NaN, got nil")
	}
}

func TestLoad_ARMRateLimitRPS_PosInf_Fails(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("ARM_RATE_LIMIT_RPS", "+Inf")

	_, err := Load()
	if err == nil {
		t.Error("expected error for ARM_RATE_LIMIT_RPS=+Inf, got nil")
	}
}

func TestLoad_ARMRateLimitRPS_NegInf_Fails(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "test-cluster")
	t.Setenv("ARM_RATE_LIMIT_RPS", "-Inf")

	_, err := Load()
	if err == nil {
		t.Error("expected error for ARM_RATE_LIMIT_RPS=-Inf, got nil")
	}
}

func TestLoadARMTuningConfig_NaN_FileFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("NaN\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("10\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_TUNING_DIR", dir)

	_, err := LoadARMTuningConfig()
	if err == nil {
		t.Error("expected error for NaN in ARM_RATE_LIMIT_RPS file, got nil")
	}
}

func TestLoadARMTuningConfig_PosInf_FileFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("+Inf\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("10\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_TUNING_DIR", dir)

	_, err := LoadARMTuningConfig()
	if err == nil {
		t.Error("expected error for +Inf in ARM_RATE_LIMIT_RPS file, got nil")
	}
}

func TestLoadARMTuningConfig_NegInf_FileFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("-Inf\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("10\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_TUNING_DIR", dir)

	_, err := LoadARMTuningConfig()
	if err == nil {
		t.Error("expected error for -Inf in ARM_RATE_LIMIT_RPS file, got nil")
	}
}

// ---------------------------------------------------------------------------
// Phase 6: LoadARMTuningForReload — per-field independent results
// ---------------------------------------------------------------------------

func TestLoadARMTuningForReload_BothValid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("30\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("15\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_TUNING_DIR", dir)

	result := LoadARMTuningForReload()
	if result.FilesAbsent {
		t.Error("expected FilesAbsent=false when both files present")
	}
	if result.ARMRateLimitRPS == nil {
		t.Fatal("expected non-nil ARMRateLimitRPS")
	}
	if *result.ARMRateLimitRPS != 30 {
		t.Errorf("ARMRateLimitRPS = %v, want 30", *result.ARMRateLimitRPS)
	}
	if result.ARMRateLimitRPSError != nil {
		t.Errorf("unexpected ARMRateLimitRPSError: %v", result.ARMRateLimitRPSError)
	}
	if result.MaxConcurrentActions == nil {
		t.Fatal("expected non-nil MaxConcurrentActions")
	}
	if *result.MaxConcurrentActions != 15 {
		t.Errorf("MaxConcurrentActions = %v, want 15", *result.MaxConcurrentActions)
	}
	if result.MaxConcurrentActionsErr != nil {
		t.Errorf("unexpected MaxConcurrentActionsErr: %v", result.MaxConcurrentActionsErr)
	}
}

func TestLoadARMTuningForReload_ValidRPS_InvalidConcurrency(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("25\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("garbage\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_TUNING_DIR", dir)

	result := LoadARMTuningForReload()
	// RPS should be valid.
	if result.ARMRateLimitRPS == nil {
		t.Fatal("expected non-nil ARMRateLimitRPS for valid field")
	}
	if *result.ARMRateLimitRPS != 25 {
		t.Errorf("ARMRateLimitRPS = %v, want 25", *result.ARMRateLimitRPS)
	}
	// Concurrency should have error.
	if result.MaxConcurrentActionsErr == nil {
		t.Error("expected MaxConcurrentActionsErr for invalid file, got nil")
	}
	if result.MaxConcurrentActions != nil {
		t.Errorf("expected nil MaxConcurrentActions for invalid value, got %v", *result.MaxConcurrentActions)
	}
}

func TestLoadARMTuningForReload_InvalidRPS_ValidConcurrency(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("not-a-number\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("12\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_TUNING_DIR", dir)

	result := LoadARMTuningForReload()
	// RPS should have error.
	if result.ARMRateLimitRPSError == nil {
		t.Error("expected ARMRateLimitRPSError for invalid file, got nil")
	}
	if result.ARMRateLimitRPS != nil {
		t.Errorf("expected nil ARMRateLimitRPS for invalid value, got %v", *result.ARMRateLimitRPS)
	}
	// Concurrency should be valid.
	if result.MaxConcurrentActions == nil {
		t.Fatal("expected non-nil MaxConcurrentActions for valid field")
	}
	if *result.MaxConcurrentActions != 12 {
		t.Errorf("MaxConcurrentActions = %v, want 12", *result.MaxConcurrentActions)
	}
}

func TestLoadARMTuningForReload_BothFilesAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ARM_TUNING_DIR", dir)

	result := LoadARMTuningForReload()
	if !result.FilesAbsent {
		t.Error("expected FilesAbsent=true when both files missing")
	}
}

func TestLoadARMTuningForReload_RPSFileMissing_ConcurrencyValid(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("8\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_TUNING_DIR", dir)

	result := LoadARMTuningForReload()
	if result.FilesAbsent {
		t.Error("expected FilesAbsent=false when one file is present")
	}
	// Missing file → error for that field.
	if result.ARMRateLimitRPSError == nil {
		t.Error("expected ARMRateLimitRPSError for missing file, got nil")
	}
	// Present file → valid value.
	if result.MaxConcurrentActions == nil {
		t.Fatal("expected non-nil MaxConcurrentActions for present valid file")
	}
	if *result.MaxConcurrentActions != 8 {
		t.Errorf("MaxConcurrentActions = %v, want 8", *result.MaxConcurrentActions)
	}
}

func TestLoadARMTuningForReload_NaN_RPS_Rejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("NaN\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("10\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARM_TUNING_DIR", dir)

	result := LoadARMTuningForReload()
	if result.ARMRateLimitRPSError == nil {
		t.Error("expected ARMRateLimitRPSError for NaN, got nil")
	}
	if result.ARMRateLimitRPS != nil {
		t.Errorf("expected nil ARMRateLimitRPS for NaN, got %v", *result.ARMRateLimitRPS)
	}
}

func TestLoadARMTuningConfig_RuntimeReload_FilesRemovedReturnsAbsent(t *testing.T) {
	// Simulates files being removed at runtime (e.g., ConfigMap unmounted).
	dir := t.TempDir()
	t.Setenv("ARM_TUNING_DIR", dir)

	// Write files, load successfully.
	if err := os.WriteFile(dir+"/ARM_RATE_LIMIT_RPS", []byte("25\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/MAX_CONCURRENT_ACTIONS", []byte("8\n"), 0644); err != nil {
		t.Fatal(err)
	}

	tuning, err := LoadARMTuningConfig()
	if err != nil {
		t.Fatalf("initial load should succeed: %v", err)
	}
	if tuning.ARMRateLimitRPS != 25 || tuning.MaxConcurrentActions != 8 {
		t.Fatalf("unexpected initial values: %+v", tuning)
	}

	// Remove files — simulates ConfigMap deletion at runtime.
	os.Remove(dir + "/ARM_RATE_LIMIT_RPS")
	os.Remove(dir + "/MAX_CONCURRENT_ACTIONS")

	_, err = LoadARMTuningConfig()
	if err == nil {
		t.Fatal("expected error after files removed, got nil")
	}
	// Reloader sees this error and preserves last-good values.
	if !errors.Is(err, ErrTuningFilesAbsent) {
		t.Errorf("expected ErrTuningFilesAbsent for removed files, got: %v", err)
	}
}
