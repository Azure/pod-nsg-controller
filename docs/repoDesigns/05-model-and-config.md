# 05 — Model & Config Design

> Packages: `internal/model`, `internal/config` · Resource ID parsing, ownership keys, selectors, configuration

## Model Package

### Overview

The `internal/model` package provides shared domain primitives used across the controller and engine layers: ARM resource ID parsing, ownership key generation, and pod selector compilation.

### Resource ID Parsing

```go
type ParsedASGReference struct {
    SubscriptionID string
    ResourceGroup  string
    ASGName        string
    FullResourceID string  // canonical casing for static segments
}

func ParseASGResourceID(resourceID string) (ParsedASGReference, error)
```

#### Validation Rules

1. Must start with `/`
2. Must not have trailing `/`
3. Must have exactly 8 segments: `/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/applicationSecurityGroups/{name}`
4. Static segments (`subscriptions`, `resourceGroups`, `providers`, `Microsoft.Network`, `applicationSecurityGroups`) are validated case-insensitively
5. Static segments are **rebuilt with canonical casing** in `FullResourceID`
6. Dynamic segments (`{sub}`, `{rg}`, `{name}`) **preserve original casing**

#### Example

```
Input:  /Subscriptions/MY-SUB/resourcegroups/My-RG/providers/microsoft.network/applicationsecuritygroups/my-asg
Output:
  SubscriptionID: "MY-SUB"
  ResourceGroup:  "My-RG"
  ASGName:        "my-asg"
  FullResourceID: "/subscriptions/MY-SUB/resourceGroups/My-RG/providers/Microsoft.Network/applicationSecurityGroups/my-asg"
```

### Ownership Key

```go
func OwnershipKey(clusterName, namespace, mappingName string) string
```

Returns `"{clusterName}__{namespace}__{mappingName}"` — used as the `PrefixSetName` in Azure and as the identity for cleanup on deletion.

### Selector Compilation

```go
func CompileSelector(sel v1alpha1.PodSelector) (labels.Selector, error)
```

Converts the CRD `PodSelector.MatchLabels` into a Kubernetes `labels.Selector` for pod matching.

### Mapping Index

`index.go` provides a precomputed index for efficient pod-to-ASG lookups:

```go
type MappingIndex struct {
    // Precomputed selector→ASG mappings for a namespace
}

func BuildIndex(mappings []v1alpha1.PodASGMapping) *MappingIndex
```

Functions:
- `BuildIndex(mappings)` — precompiles all pod selectors and ASG references into an index
- `MatchingASGs(podLabels)` — returns ASG targets matching a pod's labels in O(n) selector evaluations
- `canonicalASGKey(ref)` — normalizes ASG reference for deduplication (case-insensitive)

The index is used by the desired-state cache for incremental pod add/update/delete operations without re-evaluating all mapping rules.

---

## Config Package

### Overview

The `internal/config` package reads all controller configuration from environment variables and validates required fields.

### Constants

```go
const (
    LabelASG       = "pod-nsg-controller.azure.com/asg"
    AnnotationASGs = "pod-nsg-controller.azure.com/asgs"
)
```

These are the pod label/annotation keys used by the legacy `PodReconciler` path.

### Config Struct

```go
type Config struct {
    SubscriptionID       string        // AZURE_SUBSCRIPTION_ID (optional)
    ResourceGroup        string        // AZURE_RESOURCE_GROUP (optional)
    ClusterName          string        // CLUSTER_NAME (required, lowercase)
    ResyncInterval       time.Duration // RESYNC_INTERVAL_SECONDS (default: 60s)
    ARMRateLimitRPS      float64       // ARM_RATE_LIMIT_RPS (default: 10)
    MaxConcurrentActions int           // MAX_CONCURRENT_ACTIONS (default: 5)
    PatchThresholdPercent int           // POD_NSG_PATCH_THRESHOLD_PERCENT (default: 50)
}
```

### Load Function

```go
func Load() (*Config, error)
```

1. Reads env vars into `Config` struct
2. Applies defaults for optional fields
3. Parses `RESYNC_INTERVAL_SECONDS` as integer → `time.Duration`
4. Parses `ARM_RATE_LIMIT_RPS` as float (must be > 0)
5. Parses `MAX_CONCURRENT_ACTIONS` as integer (must be ≥ 1)
6. Calls `Validate()` to enforce required fields

### Validation

```go
func (c *Config) Validate() error
```

- `CLUSTER_NAME` must be non-empty
- `CLUSTER_NAME` must be lowercase (Azure prefix set names are case-insensitive; lowercase prevents collisions)

### Environment Variables

| Variable | Required | Default | Validation |
|----------|----------|---------|------------|
| `CLUSTER_NAME` | Yes | — | Non-empty, lowercase |
| `AZURE_SUBSCRIPTION_ID` | No | `""` | — |
| `AZURE_RESOURCE_GROUP` | No | `""` | — |
| `RESYNC_INTERVAL_SECONDS` | No | `60` | Positive integer |
| `ARM_RATE_LIMIT_RPS` | No | `10` | Positive float |
| `MAX_CONCURRENT_ACTIONS` | No | `5` | Positive integer ≥ 1 |
| `POD_NSG_PATCH_THRESHOLD_PERCENT` | No | `50` | Patch vs full-update threshold (0-100) |

### Wiring in main.go

```go
cfg, err := config.Load()

// Used to construct:
// - MappingReconciler.ClusterName
// - MappingReconciler.ResyncInterval
// - azure.NewARMRateLimiter(log, cfg.ARMRateLimitRPS)
// - azure.NewExecutor(log, factory, cfg.MaxConcurrentActions)
// - azure.NewExecutor(..., azure.WithPatchThresholdPercent(cfg.PatchThresholdPercent))
```
