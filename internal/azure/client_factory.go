package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// wireserverCredential acquires tokens directly from the Azure wireserver
// (168.63.129.16) when the IMDS identity endpoint (169.254.169.254) is broken.
type wireserverCredential struct {
	client *http.Client
}

type wireserverTokenResponse struct {
	AccessToken  string `json:"access_token"`
	ExpiresOn    string `json:"expires_on"`
	Resource     string `json:"resource"`
	TokenType    string `json:"token_type"`
	ExpiresIn    string `json:"expires_in"`
	NotBefore    string `json:"not_before"`
	ExtExpiresIn string `json:"ext_expires_in"`
}

func (w *wireserverCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	if len(opts.Scopes) == 0 {
		return azcore.AccessToken{}, fmt.Errorf("wireserverCredential: at least one scope is required")
	}
	// Convert scope to resource (remove /.default suffix)
	resource := opts.Scopes[0]
	if len(resource) > 9 && resource[len(resource)-9:] == "/.default" {
		resource = resource[:len(resource)-9]
	}

	url := fmt.Sprintf("http://168.63.129.16/metadata/identity/oauth2/token?api-version=2018-02-01&resource=%s", resource)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return azcore.AccessToken{}, errors.Wrap(err, "wireserverCredential: creating request")
	}
	req.Header.Set("Metadata", "true")

	resp, err := w.client.Do(req)
	if err != nil {
		return azcore.AccessToken{}, errors.Wrap(err, "wireserverCredential: requesting token from wireserver")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return azcore.AccessToken{}, errors.Wrap(err, "wireserverCredential: reading response")
	}
	if resp.StatusCode != http.StatusOK {
		return azcore.AccessToken{}, fmt.Errorf("wireserverCredential: wireserver returned %d: %s", resp.StatusCode, string(body))
	}

	var tokenResp wireserverTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return azcore.AccessToken{}, errors.Wrap(err, "wireserverCredential: parsing token response")
	}

	// Parse expires_on (Unix timestamp)
	var expiresOn time.Time
	var ts int64
	if _, err := fmt.Sscanf(tokenResp.ExpiresOn, "%d", &ts); err == nil {
		expiresOn = time.Unix(ts, 0)
	} else {
		expiresOn = time.Now().Add(time.Hour)
	}

	return azcore.AccessToken{
		Token:     tokenResp.AccessToken,
		ExpiresOn: expiresOn,
	}, nil
}

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

// resolveCredential lazily resolves the credential (thread-safe).
// If the credential was explicitly provided (even as nil), resolution is skipped.
// When USE_WIRESERVER_IDENTITY=true, redirects IMDS traffic to the Azure wireserver
// to work around broken IMDS identity endpoints (410 Gone).
func (f *ClientFactory) resolveCredential() error {
	if f.credExplicit {
		return nil
	}
	f.credOnce.Do(func() {
		if f.credential != nil {
			return
		}
		if os.Getenv("USE_WIRESERVER_IDENTITY") == "true" {
			f.log.Info("USE_WIRESERVER_IDENTITY enabled, acquiring tokens directly from wireserver 168.63.129.16")
			f.credential = &wireserverCredential{
				client: &http.Client{Timeout: 30 * time.Second},
			}
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
