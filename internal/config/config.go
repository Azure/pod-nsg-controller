package config

import (
	"fmt"
	"os"
)

const (
	// LabelASG is the pod label for single-ASG assignment.
	LabelASG = "pod-nsg-controller.azure.com/asg"

	// AnnotationASGs is the pod annotation for multi-ASG assignment (comma-separated).
	AnnotationASGs = "pod-nsg-controller.azure.com/asgs"
)

// Config holds the controller configuration.
type Config struct {
	// SubscriptionID is the Azure subscription containing the NSG and ASGs.
	SubscriptionID string

	// ResourceGroup is the Azure resource group containing the NSG and ASGs.
	ResourceGroup string

	// NSGName is the name of the NSG to manage rules on.
	NSGName string
}

// Load reads configuration from environment variables and validates required fields.
func Load() (*Config, error) {
	cfg := &Config{
		SubscriptionID: os.Getenv("AZURE_SUBSCRIPTION_ID"),
		ResourceGroup:  os.Getenv("AZURE_RESOURCE_GROUP"),
		NSGName:        os.Getenv("AZURE_NSG_NAME"),
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks that all required configuration fields are set.
func (c *Config) Validate() error {
	if c.SubscriptionID == "" {
		return fmt.Errorf("AZURE_SUBSCRIPTION_ID is required")
	}
	if c.ResourceGroup == "" {
		return fmt.Errorf("AZURE_RESOURCE_GROUP is required")
	}
	if c.NSGName == "" {
		return fmt.Errorf("AZURE_NSG_NAME is required")
	}
	return nil
}
