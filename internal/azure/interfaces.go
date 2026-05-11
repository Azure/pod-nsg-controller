package azure

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// AddressPrefixSetAPI defines the interface for address prefix set operations.
type AddressPrefixSetAPI interface {
	Get(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, error)
	Put(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string) error
	Delete(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) error
	List(ctx context.Context, subscriptionID, resourceGroup, asgName string) ([]AddressPrefixSet, error)
}

// AddressPrefixSetClientFactory creates clients scoped to a subscription.
type AddressPrefixSetClientFactory interface {
	ForSubscription(subscriptionID string) (AddressPrefixSetAPI, error)
}

var (
	ErrNotFound    = errors.New("azure resource not found")
	ErrMissingETag = errors.New("missing etag for existing resource")
)

// ARMStatusError represents an Azure ARM error response.
type ARMStatusError struct {
	StatusCode int
	ARMCode    string
	Message    string
	Operation  ARMOperation
	RequestURL string
	RetryAfter time.Duration
}

func (e *ARMStatusError) Error() string {
	return fmt.Sprintf("azure ARM error: status=%d code=%s message=%s", e.StatusCode, e.ARMCode, e.Message)
}

func (e *ARMStatusError) Unwrap() error {
	if e.StatusCode == 404 {
		return ErrNotFound
	}
	return nil
}

// IsNotFound returns true if the error represents a 404 Not Found.
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound)
}

// IsPreconditionFailed returns true if the error represents a 412 Precondition Failed.
func IsPreconditionFailed(err error) bool {
	var armErr *ARMStatusError
	if errors.As(err, &armErr) {
		return armErr.StatusCode == 412
	}
	return false
}
