package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
)

const (
	// LabelASG is the pod label for single-ASG assignment.
	LabelASG = "pod-nsg-controller.azure.com/asg"

	// AnnotationASGs is the pod annotation for multi-ASG assignment (comma-separated).
	AnnotationASGs = "pod-nsg-controller.azure.com/asgs"

	defaultResyncIntervalSeconds = 60
)

// Config holds the controller configuration.
type Config struct {
	// SubscriptionID is the optional default Azure subscription.
	SubscriptionID string

	// ResourceGroup is the optional default Azure resource group.
	ResourceGroup string

	// ClusterName is the unique cluster identity used in ownership keys (required, lowercase).
	ClusterName string

	// ResyncInterval is the periodic resync interval for drift correction.
	ResyncInterval time.Duration
}

// Load reads configuration from environment variables and validates required fields.
func Load() (*Config, error) {
	cfg := &Config{
		SubscriptionID: os.Getenv("AZURE_SUBSCRIPTION_ID"),
		ResourceGroup:  os.Getenv("AZURE_RESOURCE_GROUP"),
		ClusterName:    os.Getenv("CLUSTER_NAME"),
	}

	// Parse resync interval.
	resyncStr := os.Getenv("RESYNC_INTERVAL_SECONDS")
	if resyncStr == "" {
		cfg.ResyncInterval = time.Duration(defaultResyncIntervalSeconds) * time.Second
	} else {
		val, err := strconv.Atoi(resyncStr)
		if err != nil {
			return nil, errors.Wrap(err, "RESYNC_INTERVAL_SECONDS must be a valid integer")
		}
		if val < 1 {
			return nil, fmt.Errorf("RESYNC_INTERVAL_SECONDS must be >= 1, got %d", val)
		}
		cfg.ResyncInterval = time.Duration(val) * time.Second
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Validate checks that all required configuration fields are set.
func (c *Config) Validate() error {
	if c.ClusterName == "" {
		return fmt.Errorf("CLUSTER_NAME is required")
	}
	if c.ClusterName != strings.ToLower(c.ClusterName) {
		return fmt.Errorf("CLUSTER_NAME must be lowercase to avoid Azure ownership collisions")
	}
	return nil
}
