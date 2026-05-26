package config

import (
	"testing"
	"time"
)

func TestLoad_AllSet(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-123")
	t.Setenv("AZURE_RESOURCE_GROUP", "rg-test")
	t.Setenv("CLUSTER_NAME", "cluster-a")
	t.Setenv("RESYNC_INTERVAL_SECONDS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SubscriptionID != "sub-123" {
		t.Errorf("expected SubscriptionID=sub-123, got %s", cfg.SubscriptionID)
	}
	if cfg.ResourceGroup != "rg-test" {
		t.Errorf("expected ResourceGroup=rg-test, got %s", cfg.ResourceGroup)
	}
	if cfg.ClusterName != "cluster-a" {
		t.Errorf("expected ClusterName=cluster-a, got %s", cfg.ClusterName)
	}
}

func TestLoad_AllowsMissingSubscriptionAndResourceGroup(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("AZURE_RESOURCE_GROUP", "")
	t.Setenv("CLUSTER_NAME", "cluster-a")
	t.Setenv("RESYNC_INTERVAL_SECONDS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected no error when subscription/rg are empty, got: %v", err)
	}
	if cfg.SubscriptionID != "" {
		t.Errorf("expected empty SubscriptionID, got %s", cfg.SubscriptionID)
	}
	if cfg.ResourceGroup != "" {
		t.Errorf("expected empty ResourceGroup, got %s", cfg.ResourceGroup)
	}
}

func TestLoad_MissingClusterNameFails(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-123")
	t.Setenv("AZURE_RESOURCE_GROUP", "rg-test")
	t.Setenv("CLUSTER_NAME", "")
	t.Setenv("RESYNC_INTERVAL_SECONDS", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing CLUSTER_NAME")
	}
}

func TestLoad_ClusterNameMustBeLowercase(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-123")
	t.Setenv("AZURE_RESOURCE_GROUP", "rg-test")
	t.Setenv("CLUSTER_NAME", "Prod-Cluster")
	t.Setenv("RESYNC_INTERVAL_SECONDS", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for non-lowercase CLUSTER_NAME")
	}
}

func TestLoad_ResyncIntervalDefault60(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("AZURE_RESOURCE_GROUP", "")
	t.Setenv("CLUSTER_NAME", "cluster-a")
	t.Setenv("RESYNC_INTERVAL_SECONDS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ResyncInterval != 60*time.Second {
		t.Errorf("expected ResyncInterval=60s, got %v", cfg.ResyncInterval)
	}
}

func TestLoad_ResyncIntervalInvalidFails(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("AZURE_RESOURCE_GROUP", "")
	t.Setenv("CLUSTER_NAME", "cluster-a")
	t.Setenv("RESYNC_INTERVAL_SECONDS", "abc")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid RESYNC_INTERVAL_SECONDS")
	}
}

func TestLoad_ResyncIntervalLessThanOneFails(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("AZURE_RESOURCE_GROUP", "")
	t.Setenv("CLUSTER_NAME", "cluster-a")
	t.Setenv("RESYNC_INTERVAL_SECONDS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for RESYNC_INTERVAL_SECONDS < 1")
	}
}

// ---------------------------------------------------------------------------
// TestLoad_MaxConcurrentReconciles: table-driven coverage for MAX_CONCURRENT_RECONCILES
// ---------------------------------------------------------------------------
func TestLoad_MaxConcurrentReconciles(t *testing.T) {
	tests := []struct {
		name      string
		envValue  string
		wantValue int
		wantErr   bool
	}{
		{
			name:      "explicit value 10",
			envValue:  "10",
			wantValue: 10,
			wantErr:   false,
		},
		{
			name:      "unset defaults to 5",
			envValue:  "",
			wantValue: 5,
			wantErr:   false,
		},
		{
			name:    "zero is invalid",
			envValue: "0",
			wantErr: true,
		},
		{
			name:    "negative is invalid",
			envValue: "-1",
			wantErr: true,
		},
		{
			name:    "non-integer is invalid",
			envValue: "abc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AZURE_SUBSCRIPTION_ID", "")
			t.Setenv("AZURE_RESOURCE_GROUP", "")
			t.Setenv("CLUSTER_NAME", "cluster-a")
			t.Setenv("RESYNC_INTERVAL_SECONDS", "")
			t.Setenv("MAX_CONCURRENT_RECONCILES", tt.envValue)

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for MAX_CONCURRENT_RECONCILES=%q, got nil", tt.envValue)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.MaxConcurrentReconciles != tt.wantValue {
				t.Errorf("MaxConcurrentReconciles = %d, want %d", cfg.MaxConcurrentReconciles, tt.wantValue)
			}
		})
	}
}

func TestLoad_AZURENSGNAMEIgnored(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-123")
	t.Setenv("AZURE_RESOURCE_GROUP", "rg-test")
	t.Setenv("AZURE_NSG_NAME", "some-nsg")
	t.Setenv("CLUSTER_NAME", "cluster-a")
	t.Setenv("RESYNC_INTERVAL_SECONDS", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("expected no error when AZURE_NSG_NAME is set, got: %v", err)
	}
	if cfg.ClusterName != "cluster-a" {
		t.Errorf("expected ClusterName=cluster-a, got %s", cfg.ClusterName)
	}
}

// ---------------------------------------------------------------------------
// TestLoad_MaxConcurrentAzureReads: table-driven coverage for MAX_CONCURRENT_AZURE_READS
// ---------------------------------------------------------------------------
func TestLoad_MaxConcurrentAzureReads(t *testing.T) {
	tests := []struct {
		name      string
		envValue  string
		wantValue int
		wantErr   bool
	}{
		{
			name:      "explicit value 20",
			envValue:  "20",
			wantValue: 20,
		},
		{
			name:      "unset defaults to 10",
			envValue:  "",
			wantValue: 10,
		},
		{
			name:    "zero is invalid",
			envValue: "0",
			wantErr: true,
		},
		{
			name:    "negative is invalid",
			envValue: "-1",
			wantErr: true,
		},
		{
			name:    "non-integer is invalid",
			envValue: "abc",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AZURE_SUBSCRIPTION_ID", "")
			t.Setenv("AZURE_RESOURCE_GROUP", "")
			t.Setenv("CLUSTER_NAME", "cluster-a")
			t.Setenv("RESYNC_INTERVAL_SECONDS", "")
			t.Setenv("MAX_CONCURRENT_AZURE_READS", tt.envValue)

			cfg, err := Load()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for MAX_CONCURRENT_AZURE_READS=%q, got nil", tt.envValue)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.MaxConcurrentAzureReads != tt.wantValue {
				t.Errorf("MaxConcurrentAzureReads = %d, want %d", cfg.MaxConcurrentAzureReads, tt.wantValue)
			}
		})
	}
}
