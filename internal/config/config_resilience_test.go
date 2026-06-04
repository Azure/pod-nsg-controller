package config

import (
	"os"
	"testing"
)

// ---------- ARMRateLimitRPS ----------

func TestLoad_ARMRateLimitRPS_Default10(t *testing.T) {
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

func TestLoad_MaxConcurrentActions_Default5(t *testing.T) {
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
