package fake

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Azure/pod-nsg-controller/internal/azure"
)

// Operation identifies an API operation for error injection.
type Operation string

const (
	OperationGet    Operation = "GET"
	OperationPut    Operation = "PUT"
	OperationDelete Operation = "DELETE"
	OperationList   Operation = "LIST"
)

// InjectKey identifies a specific operation+resource for error injection.
type InjectKey struct {
	Operation      Operation
	SubscriptionID string
	ResourceGroup  string
	ASGName        string
	PrefixSetName  string // empty for LIST
}

// resourceKey creates a unique key for a prefix set resource.
func resourceKey(subscriptionID, resourceGroup, asgName, prefixSetName string) string {
	return subscriptionID + "/" + resourceGroup + "/" + asgName + "/" + prefixSetName
}

// PrefixSetEntry stores fake state for a single prefix set.
type PrefixSetEntry struct {
	IPs     []string
	ETag    string
	Version int64
}

// Client is an in-memory fake implementation of azure.AddressPrefixSetAPI.
type Client struct {
	mu    sync.RWMutex
	store map[string]*PrefixSetEntry

	// Legacy simple 412 injection (kept for backward-compat with existing tests).
	Fail412Count int
	fail412Calls int

	// Deterministic per-key error injection.
	injectedErrors map[InjectKey][]error
}

// NewClient creates a new fake Client.
func NewClient() *Client {
	return &Client{
		store:          make(map[string]*PrefixSetEntry),
		injectedErrors: make(map[InjectKey][]error),
	}
}

// InjectError queues count copies of err for the given key (FIFO, consumed atomically).
func (c *Client) InjectError(key InjectKey, err error, count int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := 0; i < count; i++ {
		c.injectedErrors[key] = append(c.injectedErrors[key], err)
	}
	return nil
}

// ClearInjectedErrors removes all queued error injections.
func (c *Client) ClearInjectedErrors() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.injectedErrors = make(map[InjectKey][]error)
}

// consumeInjectedError pops the first error for a key, if any. Must be called under write lock.
func (c *Client) consumeInjectedError(key InjectKey) error {
	errs := c.injectedErrors[key]
	if len(errs) == 0 {
		return nil
	}
	c.injectedErrors[key] = errs[1:]
	if len(c.injectedErrors[key]) == 0 {
		delete(c.injectedErrors, key)
	}
	return errs[0]
}

func (c *Client) Get(_ context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*azure.AddressPrefixSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check per-key error injection first
	injKey := InjectKey{
		Operation:      OperationGet,
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ASGName:        asgName,
		PrefixSetName:  prefixSetName,
	}
	if injErr := c.consumeInjectedError(injKey); injErr != nil {
		return nil, injErr
	}

	key := resourceKey(subscriptionID, resourceGroup, asgName, prefixSetName)
	entry, ok := c.store[key]
	if !ok {
		return nil, azure.ErrNotFound
	}
	name := prefixSetName
	etag := entry.ETag
	ipsCopy := make([]string, len(entry.IPs))
	copy(ipsCopy, entry.IPs)
	return &azure.AddressPrefixSet{
		Name: &name,
		Etag: &etag,
		Properties: &azure.AddressPrefixSetProperties{
			AddressPrefixes: ipsCopy,
		},
	}, nil
}

func (c *Client) Put(_ context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check per-key error injection BEFORE any mutation
	injKey := InjectKey{
		Operation:      OperationPut,
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ASGName:        asgName,
		PrefixSetName:  prefixSetName,
	}
	if injErr := c.consumeInjectedError(injKey); injErr != nil {
		return injErr
	}

	// Legacy simple 412 injection
	c.fail412Calls++
	if c.fail412Calls <= c.Fail412Count {
		return &azure.ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "etag mismatch"}
	}

	key := resourceKey(subscriptionID, resourceGroup, asgName, prefixSetName)
	entry := c.store[key]
	var version int64
	if entry != nil {
		version = entry.Version
	}
	version++

	ipsCopy := make([]string, len(ips))
	copy(ipsCopy, ips)

	c.store[key] = &PrefixSetEntry{
		IPs:     ipsCopy,
		ETag:    fmt.Sprintf(`"v-%d"`, version),
		Version: version,
	}
	return nil
}

func (c *Client) Delete(_ context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check per-key error injection first
	injKey := InjectKey{
		Operation:      OperationDelete,
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ASGName:        asgName,
		PrefixSetName:  prefixSetName,
	}
	if injErr := c.consumeInjectedError(injKey); injErr != nil {
		return injErr
	}

	key := resourceKey(subscriptionID, resourceGroup, asgName, prefixSetName)
	delete(c.store, key)
	return nil
}

func (c *Client) List(_ context.Context, subscriptionID, resourceGroup, asgName string) ([]azure.AddressPrefixSet, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Check per-key error injection
	injKey := InjectKey{
		Operation:      OperationList,
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ASGName:        asgName,
	}
	if injErr := c.consumeInjectedError(injKey); injErr != nil {
		return nil, injErr
	}

	prefix := subscriptionID + "/" + resourceGroup + "/" + asgName + "/"
	var result []azure.AddressPrefixSet
	for k, entry := range c.store {
		if strings.HasPrefix(k, prefix) {
			name := strings.TrimPrefix(k, prefix)
			etag := entry.ETag
			ipsCopy := make([]string, len(entry.IPs))
			copy(ipsCopy, entry.IPs)
			result = append(result, azure.AddressPrefixSet{
				Name: &name,
				Etag: &etag,
				Properties: &azure.AddressPrefixSetProperties{
					AddressPrefixes: ipsCopy,
				},
			})
		}
	}
	return result, nil
}

// ClientFactory is a fake factory that returns pre-configured fake clients per subscription.
type ClientFactory struct {
	mu      sync.RWMutex
	clients map[string]*Client
}

// NewClientFactory creates a new fake ClientFactory.
func NewClientFactory() *ClientFactory {
	return &ClientFactory{
		clients: make(map[string]*Client),
	}
}

// RegisterClient adds a fake client for a subscription.
func (f *ClientFactory) RegisterClient(subscriptionID string, client *Client) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clients[subscriptionID] = client
}

// ForSubscription returns the fake client for the given subscription.
func (f *ClientFactory) ForSubscription(subscriptionID string) (azure.AddressPrefixSetAPI, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	client, ok := f.clients[subscriptionID]
	if !ok {
		return nil, fmt.Errorf("no client for subscription %q", subscriptionID)
	}
	return client, nil
}
