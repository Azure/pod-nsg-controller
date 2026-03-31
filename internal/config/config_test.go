package config

import (
	"testing"
)

func TestLoad_AllSet(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-123")
	t.Setenv("AZURE_RESOURCE_GROUP", "rg-test")
	t.Setenv("AZURE_NSG_NAME", "nsg-test")

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
	if cfg.NSGName != "nsg-test" {
		t.Errorf("expected NSGName=nsg-test, got %s", cfg.NSGName)
	}
}

func TestLoad_MissingSubscriptionID(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("AZURE_RESOURCE_GROUP", "rg-test")
	t.Setenv("AZURE_NSG_NAME", "nsg-test")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing AZURE_SUBSCRIPTION_ID")
	}
}

func TestLoad_MissingResourceGroup(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-123")
	t.Setenv("AZURE_RESOURCE_GROUP", "")
	t.Setenv("AZURE_NSG_NAME", "nsg-test")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing AZURE_RESOURCE_GROUP")
	}
}

func TestLoad_MissingNSGName(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "sub-123")
	t.Setenv("AZURE_RESOURCE_GROUP", "rg-test")
	t.Setenv("AZURE_NSG_NAME", "")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing AZURE_NSG_NAME")
	}
}
