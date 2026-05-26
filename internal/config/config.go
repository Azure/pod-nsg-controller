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

	// ARMRateLimitRPS is the per-subscription ARM call rate limit (default: 10).
	ARMRateLimitRPS float64

	// MaxConcurrentActions is the max parallel ARM mutations per reconcile (default: 5).
	MaxConcurrentActions int

	// MaxConcurrentReconciles is the number of concurrent reconcile workers (default: 5).
	MaxConcurrentReconciles int

	// MaxConcurrentAzureReads is the max parallel ARM GET calls across all reconciles (default: 10).
	MaxConcurrentAzureReads int
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

	// Parse ARM rate limit RPS.
	rpsStr := os.Getenv("ARM_RATE_LIMIT_RPS")
	if rpsStr == "" {
		cfg.ARMRateLimitRPS = 10.0
	} else {
		val, err := strconv.ParseFloat(rpsStr, 64)
		if err != nil {
			return nil, errors.Wrap(err, "ARM_RATE_LIMIT_RPS must be a valid number")
		}
		if val <= 0 {
			return nil, fmt.Errorf("ARM_RATE_LIMIT_RPS must be > 0, got %v", val)
		}
		cfg.ARMRateLimitRPS = val
	}

	// Parse max concurrent actions.
	concStr := os.Getenv("MAX_CONCURRENT_ACTIONS")
	if concStr == "" {
		cfg.MaxConcurrentActions = 5
	} else {
		val, err := strconv.Atoi(concStr)
		if err != nil {
			return nil, errors.Wrap(err, "MAX_CONCURRENT_ACTIONS must be a valid integer")
		}
		if val < 1 {
			return nil, fmt.Errorf("MAX_CONCURRENT_ACTIONS must be >= 1, got %d", val)
		}
		cfg.MaxConcurrentActions = val
	}

	// Parse max concurrent reconciles.
	mcrStr := os.Getenv("MAX_CONCURRENT_RECONCILES")
	if mcrStr == "" {
		cfg.MaxConcurrentReconciles = 5
	} else {
		val, err := strconv.Atoi(mcrStr)
		if err != nil {
			return nil, errors.Wrap(err, "MAX_CONCURRENT_RECONCILES must be a valid integer")
		}
		if val < 1 {
			return nil, fmt.Errorf("MAX_CONCURRENT_RECONCILES must be >= 1, got %d", val)
		}
		cfg.MaxConcurrentReconciles = val
	}

	// Parse max concurrent Azure reads.
	marStr := os.Getenv("MAX_CONCURRENT_AZURE_READS")
	if marStr == "" {
		cfg.MaxConcurrentAzureReads = 10
	} else {
		val, err := strconv.Atoi(marStr)
		if err != nil {
			return nil, errors.Wrap(err, "MAX_CONCURRENT_AZURE_READS must be a valid integer")
		}
		if val < 1 {
			return nil, fmt.Errorf("MAX_CONCURRENT_AZURE_READS must be >= 1, got %d", val)
		}
		cfg.MaxConcurrentAzureReads = val
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
