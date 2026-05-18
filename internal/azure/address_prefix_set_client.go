package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/pkg/errors"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"go.uber.org/zap"
)

const (
	apiVersion  = "2025-07-01"
	armEndpoint = "https://management.azure.com"

	// tokenRefreshMargin is how long before expiry we proactively refresh the
	// cached token, matching the Azure SDK's default behaviour.
	tokenRefreshMargin = 5 * time.Minute
)

// AddressPrefixSet represents an address prefix set child resource of an ASG.
type AddressPrefixSet struct {
	ID         *string                     `json:"id,omitempty"`
	Name       *string                     `json:"name,omitempty"`
	Etag       *string                     `json:"etag,omitempty"`
	Type       *string                     `json:"type,omitempty"`
	Properties *AddressPrefixSetProperties `json:"properties,omitempty"`
}

// AddressPrefixSetProperties contains the properties of an address prefix set.
type AddressPrefixSetProperties struct {
	AddressPrefixes   []string `json:"addressPrefixSet"` // NRP internal property name (differs from swagger "addressPrefixes")
	ProvisioningState *string  `json:"provisioningState,omitempty"`
}

// AddressPrefixSetListResult is the response for listing address prefix sets.
type AddressPrefixSetListResult struct {
	Value    []AddressPrefixSet `json:"value,omitempty"`
	NextLink *string            `json:"nextLink,omitempty"`
}

// AddressPrefixSetClient manages AddressPrefixSet operations using direct REST calls
// against the 2025-07-01 API version. It implements AddressPrefixSetAPI.
type AddressPrefixSetClient struct {
	log         *zap.Logger
	credential  azcore.TokenCredential // nil allowed for tests
	httpClient  *http.Client
	baseURL     string
	retryPolicy RetryPolicy
	rateLimiter SubscriptionRateLimiter

	// Token cache — avoids a metadata-server round trip on every request.
	tokenMu        sync.Mutex
	cachedToken    string
	tokenExpiresOn time.Time
}

// AddressPrefixSetClientOption configures an AddressPrefixSetClient.
type AddressPrefixSetClientOption func(*AddressPrefixSetClient)

// WithARMBaseURL sets the ARM base URL (used for testing with httptest servers).
func WithARMBaseURL(baseURL string) AddressPrefixSetClientOption {
	return func(c *AddressPrefixSetClient) {
		c.baseURL = baseURL
	}
}

// NewAddressPrefixSetClient creates a new AddressPrefixSetClient.
// credential == nil is allowed and treated as test-mode auth bypass.
// httpClient == nil defaults to http.DefaultClient.
func NewAddressPrefixSetClient(
	log *zap.Logger,
	credential azcore.TokenCredential,
	httpClient *http.Client,
	opts ...AddressPrefixSetClientOption,
) *AddressPrefixSetClient {
	c := &AddressPrefixSetClient{
		log:         log,
		credential:  credential,
		httpClient:  httpClient,
		baseURL:     armEndpoint,
		retryPolicy: DefaultRetryPolicy(),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.httpClient == nil {
		c.httpClient = http.DefaultClient
	}
	return c
}

func (c *AddressPrefixSetClient) resourceURL(subscriptionID, resourceGroup, asgName, prefixSetName string) string {
	return fmt.Sprintf("%s/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/%s/addressPrefixSets/%s?api-version=%s",
		c.baseURL, url.PathEscape(subscriptionID), url.PathEscape(resourceGroup), url.PathEscape(asgName), url.PathEscape(prefixSetName), apiVersion)
}

func (c *AddressPrefixSetClient) listURL(subscriptionID, resourceGroup, asgName string) string {
	return fmt.Sprintf("%s/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/%s/addressPrefixSets?api-version=%s",
		c.baseURL, url.PathEscape(subscriptionID), url.PathEscape(resourceGroup), url.PathEscape(asgName), apiVersion)
}

func (c *AddressPrefixSetClient) acquireToken(ctx context.Context) (string, error) {
	if c.credential == nil {
		return "", nil
	}

	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()

	// Return cached token if still valid (with margin before expiry).
	if c.cachedToken != "" && time.Now().Before(c.tokenExpiresOn.Add(-tokenRefreshMargin)) {
		return c.cachedToken, nil
	}

	token, err := c.credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return "", errors.Wrap(err, "acquiring token")
	}

	c.cachedToken = token.Token
	c.tokenExpiresOn = token.ExpiresOn
	return token.Token, nil
}

func (c *AddressPrefixSetClient) doRequest(ctx context.Context, retryCtx RetryContext, method, requestURL string, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	policy := c.retryPolicy

	for attempt := 1; ; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Rate limit before each attempt.
		if c.rateLimiter != nil && retryCtx.SubscriptionID != "" {
			if err := c.rateLimiter.Wait(ctx, retryCtx.SubscriptionID); err != nil {
				return nil, err
			}
		}

		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}

		req, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
		if err != nil {
			return nil, errors.Wrap(err, "creating request")
		}
		req.Header.Set("Content-Type", "application/json")

		token, err := c.acquireToken(ctx)
		if err != nil {
			return nil, err
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		for k, v := range extraHeaders {
			req.Header.Set(k, v)
		}

		resp, httpErr := c.httpClient.Do(req)
		if httpErr != nil {
			decision := DecideRetry(httpErr, attempt, policy)
			if !decision.Retry {
				return nil, errors.Wrap(httpErr, "executing request")
			}
			c.log.Warn("transient request error, retrying",
				zap.String("method", method),
				zap.String("operation", string(retryCtx.Operation)),
				zap.Int("attempt", attempt),
				zap.Int("maxRetries", policy.MaxRetries),
				zap.String("retryReason", decision.RetryReason),
				zap.Duration("retryDelay", decision.Delay),
				zap.Error(httpErr),
			)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(decision.Delay):
			}
			continue
		}

		// 2xx success.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		// Non-2xx: parse ARM error with context.
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		armErr := ParseARMErrorWithContext(resp.StatusCode, respBody, resp.Header, retryCtx)

		decision := DecideRetry(armErr, attempt, policy)
		if !decision.Retry {
			return nil, armErr
		}

		c.log.Warn("retryable ARM error, retrying",
			zap.String("method", method),
			zap.String("operation", string(retryCtx.Operation)),
			zap.Int("statusCode", resp.StatusCode),
			zap.String("armCode", armErr.ARMCode),
			zap.Int("attempt", attempt),
			zap.Int("maxRetries", policy.MaxRetries),
			zap.String("retryReason", decision.RetryReason),
			zap.Duration("retryDelay", decision.Delay),
		)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(decision.Delay):
		}
	}
}

// armErrorBody is the shape of an ARM error response.
type armErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseARMError(statusCode int, body []byte) *ARMStatusError {
	var armErr armErrorBody
	if json.Unmarshal(body, &armErr) == nil && armErr.Error.Code != "" {
		return &ARMStatusError{
			StatusCode: statusCode,
			ARMCode:    armErr.Error.Code,
			Message:    armErr.Error.Message,
		}
	}
	return &ARMStatusError{
		StatusCode: statusCode,
		ARMCode:    fmt.Sprintf("HTTP%d", statusCode),
		Message:    string(body),
	}
}

// Get returns the specified address prefix set.
func (c *AddressPrefixSetClient) Get(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) (*AddressPrefixSet, error) {
	reqURL := c.resourceURL(subscriptionID, resourceGroup, asgName, prefixSetName)

	c.log.Debug("GET AddressPrefixSet",
		zap.String("subscriptionID", subscriptionID),
		zap.String("resourceGroup", resourceGroup),
		zap.String("asgName", asgName),
		zap.String("prefixSetName", prefixSetName),
	)

	retryCtx := RetryContext{
		Operation:      ARMOperationGetPrefixSet,
		Method:         http.MethodGet,
		URL:            reqURL,
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ASGName:        asgName,
		PrefixSetName:  prefixSetName,
	}

	resp, err := c.doRequest(ctx, retryCtx, http.MethodGet, reqURL, nil, nil)
	if err != nil {
		return nil, errors.Wrap(err, "GET AddressPrefixSet")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "reading response body")
	}

	var result AddressPrefixSet
	var multipleResults bool

	// The ARM API returns a list envelope {"value":[...]} for both single-resource
	// GET and list operations. Try to extract the resource from the list first.
	var listResult AddressPrefixSetListResult
	if err := json.Unmarshal(body, &listResult); err == nil && listResult.Value != nil {
		if len(listResult.Value) == 0 {
			return nil, errors.Wrapf(ErrNotFound, "GET AddressPrefixSet %s returned empty list", prefixSetName)
		}
		// Find the matching prefix set by name — the list may contain multiple
		// prefix sets belonging to the same ASG (e.g., from different clusters).
		// Taking Value[0] without checking the name would return the wrong ETag
		// when multiple prefix sets exist, causing 412 PreconditionFailed on PUT.
		multipleResults = len(listResult.Value) > 1
		found := false
		for i := range listResult.Value {
			itemName := ""
			if listResult.Value[i].Name != nil {
				itemName = *listResult.Value[i].Name
			} else if listResult.Value[i].ID != nil {
				// Extract name from resource ID (last segment)
				parts := strings.Split(*listResult.Value[i].ID, "/")
				if len(parts) > 0 {
					itemName = parts[len(parts)-1]
				}
			}
			if strings.EqualFold(itemName, prefixSetName) {
				result = listResult.Value[i]
				found = true
				break
			}
		}
		if !found {
			return nil, errors.Wrapf(ErrNotFound, "GET AddressPrefixSet %s not found in list of %d items", prefixSetName, len(listResult.Value))
		}
	} else {
		// Fallback: try direct unmarshal (future API versions may return a single object)
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, errors.Wrap(err, "decoding response")
		}
	}

	// If the response still has no identity fields, treat as not found.
	if result.ID == nil && result.Name == nil {
		return nil, errors.Wrapf(ErrNotFound, "GET AddressPrefixSet %s returned empty resource", prefixSetName)
	}

	// Resolve ETag: when the response is a list with multiple items, the HTTP
	// header ETag refers to the list, not an individual resource — use the
	// per-resource ETag from the body. For single-resource responses (direct
	// object or list with one item), the header ETag is authoritative.
	headerETag := resp.Header.Get("ETag")
	if headerETag != "" && !multipleResults {
		result.Etag = &headerETag
	} else if (result.Etag == nil || *result.Etag == "") && headerETag != "" {
		result.Etag = &headerETag
	}

	if result.Etag == nil || *result.Etag == "" {
		return nil, errors.Wrapf(ErrMissingETag, "GET AddressPrefixSet %s", prefixSetName)
	}

	return &result, nil
}

// Put creates or updates an address prefix set using ETag-based conditional writes.
func (c *AddressPrefixSetClient) Put(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string) error {
	reqURL := c.resourceURL(subscriptionID, resourceGroup, asgName, prefixSetName)

	c.log.Debug("PUT AddressPrefixSet",
		zap.String("subscriptionID", subscriptionID),
		zap.String("resourceGroup", resourceGroup),
		zap.String("asgName", asgName),
		zap.String("prefixSetName", prefixSetName),
		zap.Int("ipCount", len(ips)),
	)

	// GET current state to determine ETag / existence
	headers := make(map[string]string)
	existing, getErr := c.Get(ctx, subscriptionID, resourceGroup, asgName, prefixSetName)
	if getErr != nil {
		if IsNotFound(getErr) {
			headers["If-None-Match"] = "*"
		} else {
			return errors.Wrap(getErr, "pre-PUT GET")
		}
	} else if existing != nil && existing.Etag != nil {
		headers["If-Match"] = *existing.Etag
	}

	if ips == nil {
		ips = []string{}
	}
	payload := AddressPrefixSet{
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: ips,
		},
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return errors.Wrap(err, "marshaling PUT body")
	}

	retryCtx := RetryContext{
		Operation:      ARMOperationPutPrefixSet,
		Method:         http.MethodPut,
		URL:            reqURL,
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ASGName:        asgName,
		PrefixSetName:  prefixSetName,
	}

	resp, err := c.doRequest(ctx, retryCtx, http.MethodPut, reqURL, bodyBytes, headers)
	if err != nil {
		return errors.Wrap(err, "PUT AddressPrefixSet")
	}
	defer resp.Body.Close()

	// Drain response body
	io.ReadAll(resp.Body)

	// Handle LRO for 201/202
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusAccepted {
		if loc := resp.Header.Get("Location"); loc != "" {
			if err := c.pollLRO(ctx, loc, subscriptionID); err != nil {
				return errors.Wrap(err, "polling PUT LRO")
			}
		}
	}

	return nil
}

// Delete removes an address prefix set. 404 is treated as success (idempotent).
func (c *AddressPrefixSetClient) Delete(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) error {
	reqURL := c.resourceURL(subscriptionID, resourceGroup, asgName, prefixSetName)

	c.log.Debug("DELETE AddressPrefixSet",
		zap.String("subscriptionID", subscriptionID),
		zap.String("resourceGroup", resourceGroup),
		zap.String("asgName", asgName),
		zap.String("prefixSetName", prefixSetName),
	)

	retryCtx := RetryContext{
		Operation:      ARMOperationDeletePrefixSet,
		Method:         http.MethodDelete,
		URL:            reqURL,
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ASGName:        asgName,
		PrefixSetName:  prefixSetName,
	}

	resp, err := c.doRequest(ctx, retryCtx, http.MethodDelete, reqURL, nil, nil)
	if err != nil {
		// 404 is success for delete (idempotent)
		if IsNotFound(err) {
			return nil
		}
		return errors.Wrap(err, "DELETE AddressPrefixSet")
	}
	defer resp.Body.Close()

	// Drain response body
	io.ReadAll(resp.Body)

	// Handle LRO for 202
	if resp.StatusCode == http.StatusAccepted {
		if loc := resp.Header.Get("Location"); loc != "" {
			if err := c.pollLRO(ctx, loc, subscriptionID); err != nil {
				return errors.Wrap(err, "polling DELETE LRO")
			}
		}
	}

	return nil
}

// List returns all address prefix sets in an ASG, following NextLink pagination.
func (c *AddressPrefixSetClient) List(ctx context.Context, subscriptionID, resourceGroup, asgName string) ([]AddressPrefixSet, error) {
	nextURL := c.listURL(subscriptionID, resourceGroup, asgName)

	c.log.Debug("LIST AddressPrefixSets",
		zap.String("subscriptionID", subscriptionID),
		zap.String("resourceGroup", resourceGroup),
		zap.String("asgName", asgName),
	)

	retryCtx := RetryContext{
		Operation:      ARMOperationListPrefixSets,
		Method:         http.MethodGet,
		URL:            nextURL,
		SubscriptionID: subscriptionID,
		ResourceGroup:  resourceGroup,
		ASGName:        asgName,
	}

	var all []AddressPrefixSet
	for nextURL != "" {
		retryCtx.URL = nextURL
		resp, err := c.doRequest(ctx, retryCtx, http.MethodGet, nextURL, nil, nil)
		if err != nil {
			return nil, errors.Wrap(err, "LIST AddressPrefixSets")
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, errors.Wrap(err, "reading LIST response body")
		}

		var result AddressPrefixSetListResult
		if err := json.Unmarshal(body, &result); err != nil {
			return nil, errors.Wrap(err, "decoding LIST response")
		}

		all = append(all, result.Value...)

		if result.NextLink != nil && *result.NextLink != "" {
			nextURL = *result.NextLink
		} else {
			nextURL = ""
		}
	}

	return all, nil
}

// pollLRO polls a Location-based LRO endpoint until it returns a terminal status.
func (c *AddressPrefixSetClient) pollLRO(ctx context.Context, location, subscriptionID string) error {
	// Resolve relative Location URLs against baseURL
	if !strings.HasPrefix(location, "http") {
		location = c.baseURL + location
	}

	retryCtx := RetryContext{
		Operation:      ARMOperationPollLRO,
		Method:         http.MethodGet,
		URL:            location,
		SubscriptionID: subscriptionID,
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}

		resp, err := c.doRequest(ctx, retryCtx, http.MethodGet, location, nil, nil)
		if err != nil {
			return errors.Wrap(err, "polling LRO")
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if resp.StatusCode == http.StatusAccepted {
			continue
		}
		return fmt.Errorf("LRO poll returned unexpected status %d", resp.StatusCode)
	}
}
