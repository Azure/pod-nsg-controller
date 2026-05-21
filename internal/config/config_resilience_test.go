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

	// Default should be 10 RPS.
	// With stub (field exists but Load doesn't parse it), ARMRateLimitRPS=0.
	if cfg.ARMRateLimitRPS != 10.0 {
		t.Errorf("expected default ARMRateLimitRPS=10, got %v", cfg.ARMRateLimitRPS)
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

	// Default should be 5.
	// With stub (field exists but Load doesn't parse it), MaxConcurrentActions=0.
	if cfg.MaxConcurrentActions != 5 {
		t.Errorf("expected default MaxConcurrentActions=5, got %d", cfg.MaxConcurrentActions)
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
