package engine

// ASGTarget identifies a unique addressPrefixSet within an Azure ASG.
type ASGTarget struct {
	SubscriptionID string
	ResourceGroup  string
	ASGName        string
	FullResourceID string
	// PrefixSetName is the ownership key / addressPrefixSet name. It is treated
	// case-insensitively as part of target identity because Azure does the same.
	PrefixSetName string
}

// DesiredPrefixSet holds the desired set of IPs for a target.
type DesiredPrefixSet struct {
	IPs map[string]struct{}
}

// ActualPrefixSet holds the current set of IPs for a target.
type ActualPrefixSet struct {
	IPs map[string]struct{}
}

// ActionKind represents the type of diff action.
type ActionKind string

const (
	CreatePrefixSet ActionKind = "CreatePrefixSet"
	UpdatePrefixSet ActionKind = "UpdatePrefixSet"
	DeletePrefixSet ActionKind = "DeletePrefixSet"
)

// Action represents a single create/update/delete operation on a prefix set.
type Action struct {
	Kind       ActionKind
	Target     ASGTarget
	DesiredIPs []string // sorted; nil for delete
}
