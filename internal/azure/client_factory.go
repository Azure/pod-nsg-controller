package azure

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// ClientFactoryOption configures a ClientFactory.
type ClientFactoryOption func(*ClientFactory)

// WithFactoryRetryPolicy sets the retry policy for clients created by the factory.
func WithFactoryRetryPolicy(p RetryPolicy) ClientFactoryOption {
	return func(f *ClientFactory) {
		f.retryPolicy = p
		f.retryPolicySet = true
	}
}

// WithFactorySubscriptionRateLimiter sets the rate limiter for clients created by the factory.
func WithFactorySubscriptionRateLimiter(l SubscriptionRateLimiter) ClientFactoryOption {
	return func(f *ClientFactory) {
		f.rateLimiter = l
	}
}

// WithFactoryARMBaseURL sets the ARM base URL for clients created by the factory.
func WithFactoryARMBaseURL(baseURL string) ClientFactoryOption {
	return func(f *ClientFactory) {
		f.baseURL = baseURL
	}
}

// ClientFactory creates and caches AddressPrefixSetAPI clients per subscription.
type ClientFactory struct {
	log        *zap.Logger
	credential azcore.TokenCredential
	httpClient *http.Client
	baseURL    string

	retryPolicy    RetryPolicy
	retryPolicySet bool
	rateLimiter    SubscriptionRateLimiter

	credExplicit bool // true if credential was explicitly provided (even if nil)
	credOnce     sync.Once
	credErr      error

	mu      sync.RWMutex
	clients map[string]AddressPrefixSetAPI
}

// NewClientFactory creates a new ClientFactory with lazy default credential resolution.
func NewClientFactory(log *zap.Logger, opts ...ClientFactoryOption) *ClientFactory {
	f := &ClientFactory{
		log:     log,
		clients: make(map[string]AddressPrefixSetAPI),
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// NewClientFactoryWithCredential creates a ClientFactory with an explicit credential (for testing).
// Passing nil disables credential resolution entirely (test-mode auth bypass).
func NewClientFactoryWithCredential(log *zap.Logger, credential azcore.TokenCredential, httpClient *http.Client, opts ...ClientFactoryOption) *ClientFactory {
	f := &ClientFactory{
		log:          log,
		credential:   credential,
		httpClient:   httpClient,
		credExplicit: true,
		clients:      make(map[string]AddressPrefixSetAPI),
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// NewClientFactoryWithDefaultCredential creates a ClientFactory using DefaultAzureCredential eagerly.
func NewClientFactoryWithDefaultCredential(log *zap.Logger, httpClient *http.Client, opts ...ClientFactoryOption) (*ClientFactory, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, errors.Wrap(err, "creating DefaultAzureCredential")
	}
	f := &ClientFactory{
		log:        log,
		credential: cred,
		httpClient: httpClient,
		clients:    make(map[string]AddressPrefixSetAPI),
	}
	for _, opt := range opts {
		opt(f)
	}
	return f, nil
}

// resolveCredential lazily resolves the DefaultAzureCredential (thread-safe).
// If the credential was explicitly provided (even as nil), resolution is skipped.
func (f *ClientFactory) resolveCredential() error {
	if f.credExplicit {
		return nil
	}
	f.credOnce.Do(func() {
		if f.credential != nil {
			return
		}
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			f.credErr = errors.Wrap(err, "creating DefaultAzureCredential")
			return
		}
		f.credential = cred
	})
	return f.credErr
}

// ForSubscription returns a cached or new client for the given subscription.
func (f *ClientFactory) ForSubscription(subscriptionID string) (AddressPrefixSetAPI, error) {
	if subscriptionID == "" {
		return nil, fmt.Errorf("subscription ID must not be empty")
	}

	f.mu.RLock()
	client, ok := f.clients[subscriptionID]
	f.mu.RUnlock()
	if ok {
		return client, nil
	}

	// Resolve credential lazily
	if err := f.resolveCredential(); err != nil {
		return nil, errors.Wrap(err, "resolving credential")
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if client, ok = f.clients[subscriptionID]; ok {
		return client, nil
	}

	var clientOpts []AddressPrefixSetClientOption
	if f.baseURL != "" {
		clientOpts = append(clientOpts, WithARMBaseURL(f.baseURL))
	}
	if f.retryPolicySet {
		clientOpts = append(clientOpts, WithRetryPolicy(f.retryPolicy))
	}
	if f.rateLimiter != nil {
		clientOpts = append(clientOpts, WithSubscriptionRateLimiter(f.rateLimiter))
	}

	client = NewAddressPrefixSetClient(
		f.log.With(zap.String("subscriptionID", subscriptionID)),
		f.credential,
		f.httpClient,
		clientOpts...,
	)
	f.clients[subscriptionID] = client
	return client, nil
}
