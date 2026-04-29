package azure

import (
	"fmt"
	"net/http"
	"sync"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"go.uber.org/zap"
)

// ClientFactoryOption configures a ClientFactory.
type ClientFactoryOption func(*ClientFactory)

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

	credOnce sync.Once
	credErr  error

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
func NewClientFactoryWithCredential(log *zap.Logger, credential azcore.TokenCredential, httpClient *http.Client, opts ...ClientFactoryOption) *ClientFactory {
	f := &ClientFactory{
		log:        log,
		credential: credential,
		httpClient: httpClient,
		clients:    make(map[string]AddressPrefixSetAPI),
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
		return nil, fmt.Errorf("creating DefaultAzureCredential: %w", err)
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
func (f *ClientFactory) resolveCredential() error {
	f.credOnce.Do(func() {
		if f.credential != nil {
			return
		}
		cred, err := azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			f.credErr = fmt.Errorf("creating DefaultAzureCredential: %w", err)
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
		return nil, fmt.Errorf("resolving credential: %w", err)
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

	client = NewAddressPrefixSetClient(
		f.log.With(zap.String("subscriptionID", subscriptionID)),
		f.credential,
		f.httpClient,
		clientOpts...,
	)
	f.clients[subscriptionID] = client
	return client, nil
}
