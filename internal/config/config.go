package config

import (
	stderrors "errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pkg/errors"
)

// ErrTuningFilesAbsent is returned by LoadARMTuningConfig when ARM_TUNING_DIR
// is set but both tuning files are missing. At startup this is non-fatal
// (caller falls back to defaults); at runtime the reloader treats it as an
// error and preserves last-good values.
var ErrTuningFilesAbsent = stderrors.New("ARM_TUNING_DIR set but both tuning files absent")

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

	// ARMRateLimitRPS is the per-subscription ARM call rate limit (default: 20).
	ARMRateLimitRPS float64

	// MaxConcurrentActions is the max parallel ARM mutations per reconcile (default: 10).
	MaxConcurrentActions int

	// MaxConcurrentReconciles is the number of concurrent reconcile workers (default: 5).
	MaxConcurrentReconciles int

	// MaxConcurrentAzureReads is the max parallel ARM GET calls across all reconciles (default: 10).
	MaxConcurrentAzureReads int

	// MinReconcileIntervalMs is the minimum interval between reconciles for the same key (default: 2000, 0 disables).
	MinReconcileIntervalMs int

	// PatchThresholdPercent controls when ComputeDiff emits PatchPrefixSet vs
	// UpdatePrefixSet. When the delta (added + removed IPs) as a percentage of
	// the set size is at or below this threshold, a patch is used. Valid range:
	// 1..100 (default: 50).
	PatchThresholdPercent int
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
	// When ARM_TUNING_DIR is set, file-backed tuning takes precedence via
	// LoadARMTuningConfig(); skip env-based validation here to avoid startup
	// failures from stale env vars that would never be used.
	if os.Getenv("ARM_TUNING_DIR") != "" {
		defaults := ARMTuningDefaults()
		cfg.ARMRateLimitRPS = defaults.ARMRateLimitRPS
		cfg.MaxConcurrentActions = defaults.MaxConcurrentActions
	} else {
		defaults := ARMTuningDefaults()
		rpsStr := os.Getenv("ARM_RATE_LIMIT_RPS")
		if rpsStr == "" {
			cfg.ARMRateLimitRPS = defaults.ARMRateLimitRPS
		} else {
			val, err := strconv.ParseFloat(rpsStr, 64)
			if err != nil {
				return nil, errors.Wrap(err, "ARM_RATE_LIMIT_RPS must be a valid number")
			}
			if err := validateARMRateLimitRPS("ARM_RATE_LIMIT_RPS", val); err != nil {
				return nil, err
			}
			cfg.ARMRateLimitRPS = val
		}

		// Parse max concurrent actions.
		concStr := os.Getenv("MAX_CONCURRENT_ACTIONS")
		if concStr == "" {
			cfg.MaxConcurrentActions = defaults.MaxConcurrentActions
		} else {
			val, err := strconv.Atoi(concStr)
			if err != nil {
				return nil, errors.Wrap(err, "MAX_CONCURRENT_ACTIONS must be a valid integer")
			}
			if err := validateMaxConcurrentActions("MAX_CONCURRENT_ACTIONS", val); err != nil {
				return nil, err
			}
			cfg.MaxConcurrentActions = val
		}
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

	// Parse min reconcile interval.
	mriStr := os.Getenv("MIN_RECONCILE_INTERVAL_MS")
	if mriStr == "" {
		cfg.MinReconcileIntervalMs = 2000
	} else {
		val, err := strconv.Atoi(mriStr)
		if err != nil {
			return nil, errors.Wrap(err, "MIN_RECONCILE_INTERVAL_MS must be a valid integer")
		}
		if val < 0 {
			return nil, fmt.Errorf("MIN_RECONCILE_INTERVAL_MS must be >= 0, got %d", val)
		}
		cfg.MinReconcileIntervalMs = val
	}

	// Parse patch threshold percent.
	ptpStr := os.Getenv("POD_NSG_PATCH_THRESHOLD_PERCENT")
	if ptpStr == "" {
		cfg.PatchThresholdPercent = 50
	} else {
		val, err := strconv.Atoi(ptpStr)
		if err != nil {
			return nil, errors.Wrap(err, "POD_NSG_PATCH_THRESHOLD_PERCENT must be a valid integer")
		}
		if val < 1 || val > 100 {
			return nil, fmt.Errorf("POD_NSG_PATCH_THRESHOLD_PERCENT must be between 1 and 100, got %d", val)
		}
		cfg.PatchThresholdPercent = val
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

// ARMTuningConfig holds runtime-tunable ARM concurrency settings.
type ARMTuningConfig struct {
	ARMRateLimitRPS      float64
	MaxConcurrentActions int
}

// ARMTuningDefaults returns ARMTuningConfig with Phase 6 default values.
func ARMTuningDefaults() ARMTuningConfig {
	return ARMTuningConfig{
		ARMRateLimitRPS:      20.0,
		MaxConcurrentActions: 10,
	}
}

// LoadARMTuningConfig loads ARM tuning from mounted files (if ARM_TUNING_DIR is set)
// or falls back to environment variables / defaults.
func LoadARMTuningConfig() (ARMTuningConfig, error) {
	tuning := ARMTuningConfig{
		ARMRateLimitRPS:      20.0,
		MaxConcurrentActions: 10,
	}

	// Try file-based tuning source first (runtime-mutable via ConfigMap mount).
	tuningDir := os.Getenv("ARM_TUNING_DIR")
	if tuningDir != "" {
		rpsBytes, rpsErr := os.ReadFile(tuningDir + "/ARM_RATE_LIMIT_RPS")
		concBytes, concErr := os.ReadFile(tuningDir + "/MAX_CONCURRENT_ACTIONS")

		// If files exist, they must be valid — no silent fallback.
		if rpsErr == nil && concErr == nil {
			rpsVal, err := strconv.ParseFloat(strings.TrimSpace(string(rpsBytes)), 64)
			if err != nil {
				return ARMTuningConfig{}, errors.Wrap(err, "ARM_TUNING_DIR/ARM_RATE_LIMIT_RPS must be a valid number")
			}
			if err := validateARMRateLimitRPS("ARM_TUNING_DIR/ARM_RATE_LIMIT_RPS", rpsVal); err != nil {
				return ARMTuningConfig{}, err
			}
			concVal, err := strconv.Atoi(strings.TrimSpace(string(concBytes)))
			if err != nil {
				return ARMTuningConfig{}, errors.Wrap(err, "ARM_TUNING_DIR/MAX_CONCURRENT_ACTIONS must be a valid integer")
			}
			if err := validateMaxConcurrentActions("ARM_TUNING_DIR/MAX_CONCURRENT_ACTIONS", concVal); err != nil {
				return ARMTuningConfig{}, err
			}
			tuning.ARMRateLimitRPS = rpsVal
			tuning.MaxConcurrentActions = concVal
			return tuning, nil
		}

		// If only one file is missing, treat as partial config error.
		if rpsErr == nil || concErr == nil {
			if rpsErr != nil {
				return ARMTuningConfig{}, errors.Wrap(rpsErr, "ARM_TUNING_DIR/ARM_RATE_LIMIT_RPS")
			}
			return ARMTuningConfig{}, errors.Wrap(concErr, "ARM_TUNING_DIR/MAX_CONCURRENT_ACTIONS")
		}
		// Both files absent — signal to caller so runtime reloader can
		// preserve last-good values. At startup, caller treats this as
		// non-fatal and falls back to env/defaults.
		// Only treat as "absent" when both errors are actually file-not-found;
		// permission errors or other I/O failures are real faults.
		if os.IsNotExist(rpsErr) && os.IsNotExist(concErr) {
			return ARMTuningConfig{}, ErrTuningFilesAbsent
		}
		// Non-ENOENT double failure — surface the real error.
		return ARMTuningConfig{}, errors.Wrap(rpsErr, "ARM_TUNING_DIR: unable to read tuning files")
	}

	// Fall back to environment variables.
	if rpsStr := os.Getenv("ARM_RATE_LIMIT_RPS"); rpsStr != "" {
		val, err := strconv.ParseFloat(rpsStr, 64)
		if err != nil {
			return ARMTuningConfig{}, errors.Wrap(err, "ARM_RATE_LIMIT_RPS must be a valid number")
		}
		if err := validateARMRateLimitRPS("ARM_RATE_LIMIT_RPS", val); err != nil {
			return ARMTuningConfig{}, err
		}
		tuning.ARMRateLimitRPS = val
	}
	if concStr := os.Getenv("MAX_CONCURRENT_ACTIONS"); concStr != "" {
		val, err := strconv.Atoi(concStr)
		if err != nil {
			return ARMTuningConfig{}, errors.Wrap(err, "MAX_CONCURRENT_ACTIONS must be a valid integer")
		}
		if err := validateMaxConcurrentActions("MAX_CONCURRENT_ACTIONS", val); err != nil {
			return ARMTuningConfig{}, err
		}
		tuning.MaxConcurrentActions = val
	}

	return tuning, nil
}

// LoadARMTuningFromEnv loads ARM tuning from environment variables only,
// falling back to Phase 6 defaults. Used at startup when tuning files are
// absent but env-based configuration should still be honoured.
func LoadARMTuningFromEnv() (ARMTuningConfig, error) {
	tuning := ARMTuningDefaults()

	if rpsStr := os.Getenv("ARM_RATE_LIMIT_RPS"); rpsStr != "" {
		val, err := strconv.ParseFloat(rpsStr, 64)
		if err != nil {
			return ARMTuningConfig{}, errors.Wrap(err, "ARM_RATE_LIMIT_RPS must be a valid number")
		}
		if err := validateARMRateLimitRPS("ARM_RATE_LIMIT_RPS", val); err != nil {
			return ARMTuningConfig{}, err
		}
		tuning.ARMRateLimitRPS = val
	}
	if concStr := os.Getenv("MAX_CONCURRENT_ACTIONS"); concStr != "" {
		val, err := strconv.Atoi(concStr)
		if err != nil {
			return ARMTuningConfig{}, errors.Wrap(err, "MAX_CONCURRENT_ACTIONS must be a valid integer")
		}
		if err := validateMaxConcurrentActions("MAX_CONCURRENT_ACTIONS", val); err != nil {
			return ARMTuningConfig{}, err
		}
		tuning.MaxConcurrentActions = val
	}

	return tuning, nil
}

// ARMTuningReloadResult holds per-field results for runtime reload.
// Unlike LoadARMTuningConfig (which is atomic), this allows applying
// valid fields independently during runtime reload.
type ARMTuningReloadResult struct {
	ARMRateLimitRPS         *float64
	MaxConcurrentActions    *int
	ARMRateLimitRPSError    error
	MaxConcurrentActionsErr error
	FilesAbsent             bool
	NotConfigured           bool // true when ARM_TUNING_DIR env var is unset
}

// LoadARMTuningForReload loads ARM tuning with per-field granularity for runtime
// reload. Each field is independently validated — a failure in one field does not
// prevent the other from being applied.
func LoadARMTuningForReload() ARMTuningReloadResult {
	tuningDir := os.Getenv("ARM_TUNING_DIR")
	if tuningDir == "" {
		return ARMTuningReloadResult{NotConfigured: true}
	}

	rpsBytes, rpsReadErr := os.ReadFile(tuningDir + "/ARM_RATE_LIMIT_RPS")
	concBytes, concReadErr := os.ReadFile(tuningDir + "/MAX_CONCURRENT_ACTIONS")

	// Both files absent.
	if rpsReadErr != nil && concReadErr != nil {
		if os.IsNotExist(rpsReadErr) && os.IsNotExist(concReadErr) {
			return ARMTuningReloadResult{FilesAbsent: true}
		}
		return ARMTuningReloadResult{
			ARMRateLimitRPSError:    errors.Wrap(rpsReadErr, "ARM_TUNING_DIR/ARM_RATE_LIMIT_RPS"),
			MaxConcurrentActionsErr: errors.Wrap(concReadErr, "ARM_TUNING_DIR/MAX_CONCURRENT_ACTIONS"),
		}
	}

	var result ARMTuningReloadResult

	// Parse RPS independently.
	if rpsReadErr != nil {
		result.ARMRateLimitRPSError = errors.Wrap(rpsReadErr, "ARM_TUNING_DIR/ARM_RATE_LIMIT_RPS")
	} else {
		rpsVal, err := strconv.ParseFloat(strings.TrimSpace(string(rpsBytes)), 64)
		if err != nil {
			result.ARMRateLimitRPSError = errors.Wrap(err, "ARM_TUNING_DIR/ARM_RATE_LIMIT_RPS must be a valid number")
		} else if err := validateARMRateLimitRPS("ARM_TUNING_DIR/ARM_RATE_LIMIT_RPS", rpsVal); err != nil {
			result.ARMRateLimitRPSError = err
		} else {
			result.ARMRateLimitRPS = &rpsVal
		}
	}

	// Parse concurrency independently.
	if concReadErr != nil {
		result.MaxConcurrentActionsErr = errors.Wrap(concReadErr, "ARM_TUNING_DIR/MAX_CONCURRENT_ACTIONS")
	} else {
		concVal, err := strconv.Atoi(strings.TrimSpace(string(concBytes)))
		if err != nil {
			result.MaxConcurrentActionsErr = errors.Wrap(err, "ARM_TUNING_DIR/MAX_CONCURRENT_ACTIONS must be a valid integer")
		} else if err := validateMaxConcurrentActions("ARM_TUNING_DIR/MAX_CONCURRENT_ACTIONS", concVal); err != nil {
			result.MaxConcurrentActionsErr = err
		} else {
			result.MaxConcurrentActions = &concVal
		}
	}

	return result
}

// validateARMRateLimitRPS checks that rps is a positive finite number.
func validateARMRateLimitRPS(field string, v float64) error {
	if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
		return fmt.Errorf("%s must be a finite number > 0, got %v", field, v)
	}
	return nil
}

// validateMaxConcurrentActions checks that concurrency is >= 1.
func validateMaxConcurrentActions(field string, v int) error {
	if v < 1 {
		return fmt.Errorf("%s must be >= 1, got %d", field, v)
	}
	return nil
}
