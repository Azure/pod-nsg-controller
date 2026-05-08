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
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

const (
	apiVersion2026 = "2026-01-01"
	armEndpoint    = "https://management.azure.com"

	// defaultMaxRetries is the number of retries for transient HTTP failures.
	defaultMaxRetries = 3
	// defaultRetryBackoff is the fixed delay between retries.
	defaultRetryBackoff = 200 * time.Millisecond
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
	AddressPrefixes   []string `json:"addressPrefixes"` // no omitempty (T4.10)
	ProvisioningState *string  `json:"provisioningState,omitempty"`
}

// AddressPrefixSetListResult is the response for listing address prefix sets.
type AddressPrefixSetListResult struct {
	Value    []AddressPrefixSet `json:"value,omitempty"`
	NextLink *string            `json:"nextLink,omitempty"`
}

// AddressPrefixSetClient manages AddressPrefixSet operations using direct REST calls
// against the 2026-01-01 API version. It implements AddressPrefixSetAPI.
type AddressPrefixSetClient struct {
	log        *zap.Logger
	credential azcore.TokenCredential // nil allowed for tests
	httpClient *http.Client
	baseURL    string
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
		log:        log,
		credential: credential,
		httpClient: httpClient,
		baseURL:    armEndpoint,
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
		c.baseURL, url.PathEscape(subscriptionID), url.PathEscape(resourceGroup), url.PathEscape(asgName), url.PathEscape(prefixSetName), apiVersion2026)
}

func (c *AddressPrefixSetClient) listURL(subscriptionID, resourceGroup, asgName string) string {
	return fmt.Sprintf("%s/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/%s/addressPrefixSets?api-version=%s",
		c.baseURL, url.PathEscape(subscriptionID), url.PathEscape(resourceGroup), url.PathEscape(asgName), apiVersion2026)
}

func (c *AddressPrefixSetClient) acquireToken(ctx context.Context) (string, error) {
	if c.credential == nil {
		return "", nil
	}
	token, err := c.credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return "", errors.Wrap(err, "acquiring token")
	}
	return token.Token, nil
}

func (c *AddressPrefixSetClient) doRequest(ctx context.Context, method, url string, body []byte, extraHeaders map[string]string) (*http.Response, error) {
	var lastResp *http.Response
	var lastErr error
	for attempt := 0; attempt <= defaultMaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(defaultRetryBackoff):
			}
		}

		var bodyReader io.Reader
		if body != nil {
			bodyReader = bytes.NewReader(body)
		}

		req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
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

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = errors.Wrap(err, "executing request")
			lastResp = nil
			c.log.Warn("transient request error, retrying",
				zap.String("method", method),
				zap.Int("attempt", attempt+1),
				zap.Error(err),
			)
			continue
		}

		if isRetryableStatus(resp.StatusCode) {
			c.log.Warn("retryable status, retrying",
				zap.String("method", method),
				zap.Int("statusCode", resp.StatusCode),
				zap.Int("attempt", attempt+1),
			)
			// Keep last response for final return; close previous if any
			if lastResp != nil {
				io.Copy(io.Discard, lastResp.Body)
				lastResp.Body.Close()
			}
			lastResp = resp
			lastErr = nil
			continue
		}

		return resp, nil
	}
	// Retries exhausted: return last response so caller can parse the ARM error
	if lastResp != nil {
		return lastResp, nil
	}
	return nil, errors.Wrap(lastErr, "all retries exhausted")
}

// isRetryableStatus returns true for HTTP status codes that indicate a transient failure.
func isRetryableStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests ||
		statusCode == http.StatusInternalServerError ||
		statusCode == http.StatusBadGateway ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout
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
	url := c.resourceURL(subscriptionID, resourceGroup, asgName, prefixSetName)

	c.log.Debug("GET AddressPrefixSet",
		zap.String("subscriptionID", subscriptionID),
		zap.String("resourceGroup", resourceGroup),
		zap.String("asgName", asgName),
		zap.String("prefixSetName", prefixSetName),
	)

	resp, err := c.doRequest(ctx, http.MethodGet, url, nil, nil)
	if err != nil {
		return nil, errors.Wrap(err, "GET AddressPrefixSet")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "reading response body")
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		armErr := parseARMError(resp.StatusCode, body)
		return nil, errors.Wrap(armErr, "GET AddressPrefixSet")
	}

	var result AddressPrefixSet
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, errors.Wrap(err, "decoding response")
	}

	// Resolve ETag: header takes precedence over body
	headerETag := resp.Header.Get("ETag")
	if headerETag != "" {
		result.Etag = &headerETag
	}

	if result.Etag == nil || *result.Etag == "" {
		return nil, errors.Wrapf(ErrMissingETag, "GET AddressPrefixSet %s", prefixSetName)
	}

	return &result, nil
}

// Put creates or updates an address prefix set using ETag-based conditional writes.
func (c *AddressPrefixSetClient) Put(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string, ips []string) error {
	url := c.resourceURL(subscriptionID, resourceGroup, asgName, prefixSetName)

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

	resp, err := c.doRequest(ctx, http.MethodPut, url, bodyBytes, headers)
	if err != nil {
		return errors.Wrap(err, "PUT AddressPrefixSet")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "reading PUT response body")
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		armErr := parseARMError(resp.StatusCode, body)
		return errors.Wrap(armErr, "PUT AddressPrefixSet")
	}

	// Handle LRO for 201/202
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusAccepted {
		if loc := resp.Header.Get("Location"); loc != "" {
			if err := c.pollLRO(ctx, loc); err != nil {
				return errors.Wrap(err, "polling PUT LRO")
			}
		}
	}

	return nil
}

// Delete removes an address prefix set. 404 is treated as success (idempotent).
func (c *AddressPrefixSetClient) Delete(ctx context.Context, subscriptionID, resourceGroup, asgName, prefixSetName string) error {
	url := c.resourceURL(subscriptionID, resourceGroup, asgName, prefixSetName)

	c.log.Debug("DELETE AddressPrefixSet",
		zap.String("subscriptionID", subscriptionID),
		zap.String("resourceGroup", resourceGroup),
		zap.String("asgName", asgName),
		zap.String("prefixSetName", prefixSetName),
	)

	resp, err := c.doRequest(ctx, http.MethodDelete, url, nil, nil)
	if err != nil {
		return errors.Wrap(err, "DELETE AddressPrefixSet")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "reading DELETE response body")
	}

	// 404 is success for delete (idempotent)
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		armErr := parseARMError(resp.StatusCode, body)
		return errors.Wrap(armErr, "DELETE AddressPrefixSet")
	}

	// Handle LRO for 202
	if resp.StatusCode == http.StatusAccepted {
		if loc := resp.Header.Get("Location"); loc != "" {
			if err := c.pollLRO(ctx, loc); err != nil {
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

	var all []AddressPrefixSet
	for nextURL != "" {
		resp, err := c.doRequest(ctx, http.MethodGet, nextURL, nil, nil)
		if err != nil {
			return nil, errors.Wrap(err, "LIST AddressPrefixSets")
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, errors.Wrap(err, "reading LIST response body")
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			armErr := parseARMError(resp.StatusCode, body)
			return nil, errors.Wrap(armErr, "LIST AddressPrefixSets")
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
func (c *AddressPrefixSetClient) pollLRO(ctx context.Context, location string) error {
	// Resolve relative Location URLs against baseURL
	if !strings.HasPrefix(location, "http") {
		location = c.baseURL + location
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}

		resp, err := c.doRequest(ctx, http.MethodGet, location, nil, nil)
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
