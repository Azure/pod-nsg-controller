package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/go-logr/logr"
)

const (
	apiVersion2026 = "2026-01-01"
	armEndpoint    = "https://management.azure.com"
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
	AddressPrefixes   []string `json:"addressPrefixes,omitempty"`
	ProvisioningState *string  `json:"provisioningState,omitempty"`
}

// AddressPrefixSetListResult is the response for listing address prefix sets.
type AddressPrefixSetListResult struct {
	Value    []AddressPrefixSet `json:"value,omitempty"`
	NextLink *string            `json:"nextLink,omitempty"`
}

// AddressPrefixSetClient manages AddressPrefixSet operations using direct REST calls
// against the 2026-01-01 API version.
type AddressPrefixSetClient struct {
	credential     azcore.TokenCredential
	subscriptionID string
	resourceGroup  string
	log            logr.Logger
}

// NewAddressPrefixSetClient creates a new AddressPrefixSetClient using DefaultAzureCredential.
func NewAddressPrefixSetClient(subscriptionID, resourceGroup string, logger logr.Logger) (*AddressPrefixSetClient, error) {
	log := logger.WithName("AddressPrefixSetClient")
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("creating Azure credential: %w", err)
	}

	log.Info("client initialized", "subscriptionID", subscriptionID, "resourceGroup", resourceGroup,
		"apiVersion", apiVersion2026)

	return &AddressPrefixSetClient{
		credential:     cred,
		subscriptionID: subscriptionID,
		resourceGroup:  resourceGroup,
		log:            log,
	}, nil
}

// Get returns the specified address prefix set within an ASG.
func (c *AddressPrefixSetClient) Get(ctx context.Context, asgName, prefixSetName string) (*AddressPrefixSet, error) {
	c.log.Info("GET AddressPrefixSet", "asgName", asgName, "prefixSetName", prefixSetName)

	path := c.resourcePath(asgName, prefixSetName)
	resp, err := c.doRequest(ctx, http.MethodGet, path, nil)
	if err != nil {
		c.log.Error(err, "GET AddressPrefixSet request failed", "asgName", asgName, "prefixSetName", prefixSetName)
		return nil, fmt.Errorf("getting address prefix set %q in ASG %q: %w", prefixSetName, asgName, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	c.log.Info("GET AddressPrefixSet response", "statusCode", resp.StatusCode, "body", string(body))

	if err := checkStatusCode(resp.StatusCode, body, http.StatusOK); err != nil {
		return nil, err
	}

	var result AddressPrefixSet
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &result, nil
}

// CreateOrUpdate creates or updates an address prefix set within an ASG.
// This is a long-running operation; the method polls until completion.
func (c *AddressPrefixSetClient) CreateOrUpdate(ctx context.Context, asgName, prefixSetName string, prefixes []string) (*AddressPrefixSet, error) {
	c.log.Info("PUT AddressPrefixSet", "asgName", asgName, "prefixSetName", prefixSetName,
		"prefixes", prefixes)

	path := c.resourcePath(asgName, prefixSetName)
	reqBody := AddressPrefixSet{
		Properties: &AddressPrefixSetProperties{
			AddressPrefixes: prefixes,
		},
	}

	resp, err := c.doRequest(ctx, http.MethodPut, path, reqBody)
	if err != nil {
		c.log.Error(err, "PUT AddressPrefixSet request failed", "asgName", asgName, "prefixSetName", prefixSetName)
		return nil, fmt.Errorf("creating/updating address prefix set %q in ASG %q: %w", prefixSetName, asgName, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	c.log.Info("PUT AddressPrefixSet response", "statusCode", resp.StatusCode, "body", string(body))

	if err := checkStatusCode(resp.StatusCode, body, http.StatusOK, http.StatusCreated); err != nil {
		return nil, err
	}

	// Poll for LRO completion if we got a 201.
	if resp.StatusCode == http.StatusCreated {
		c.log.Info("PUT AddressPrefixSet polling LRO", "prefixSetName", prefixSetName)
		if err := c.pollLRO(ctx, resp); err != nil {
			c.log.Error(err, "PUT AddressPrefixSet LRO polling failed", "prefixSetName", prefixSetName)
			return nil, fmt.Errorf("polling create for address prefix set %q: %w", prefixSetName, err)
		}
		return c.Get(ctx, asgName, prefixSetName)
	}

	var result AddressPrefixSet
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	c.log.Info("PUT AddressPrefixSet succeeded", "prefixSetName", prefixSetName, "id", ptrVal(result.ID))
	return &result, nil
}

// Delete removes an address prefix set from an ASG.
// This is a long-running operation; the method polls until completion.
func (c *AddressPrefixSetClient) Delete(ctx context.Context, asgName, prefixSetName string) error {
	c.log.Info("DELETE AddressPrefixSet", "asgName", asgName, "prefixSetName", prefixSetName)

	path := c.resourcePath(asgName, prefixSetName)
	resp, err := c.doRequest(ctx, http.MethodDelete, path, nil)
	if err != nil {
		c.log.Error(err, "DELETE AddressPrefixSet request failed", "asgName", asgName, "prefixSetName", prefixSetName)
		return fmt.Errorf("deleting address prefix set %q in ASG %q: %w", prefixSetName, asgName, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	c.log.Info("DELETE AddressPrefixSet response", "statusCode", resp.StatusCode, "body", string(body))

	if err := checkStatusCode(resp.StatusCode, body, http.StatusOK, http.StatusAccepted, http.StatusNoContent); err != nil {
		return err
	}

	// Poll for LRO completion if we got a 202.
	if resp.StatusCode == http.StatusAccepted {
		c.log.Info("DELETE AddressPrefixSet polling LRO", "prefixSetName", prefixSetName)
		if err := c.pollLRO(ctx, resp); err != nil {
			c.log.Error(err, "DELETE AddressPrefixSet LRO polling failed", "prefixSetName", prefixSetName)
			return fmt.Errorf("polling delete for address prefix set %q: %w", prefixSetName, err)
		}
	}

	c.log.Info("DELETE AddressPrefixSet succeeded", "asgName", asgName, "prefixSetName", prefixSetName)
	return nil
}

// List returns all address prefix sets in the specified ASG.
func (c *AddressPrefixSetClient) List(ctx context.Context, asgName string) ([]AddressPrefixSet, error) {
	c.log.Info("LIST AddressPrefixSets", "asgName", asgName)

	path := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/%s/listAddressPrefixSets",
		url.PathEscape(c.subscriptionID),
		url.PathEscape(c.resourceGroup),
		url.PathEscape(asgName),
	)

	var allResults []AddressPrefixSet
	currentPath := path
	pageNum := 0

	for {
		pageNum++
		resp, err := c.doRequest(ctx, http.MethodGet, currentPath, nil)
		if err != nil {
			c.log.Error(err, "LIST AddressPrefixSets request failed", "asgName", asgName, "page", pageNum)
			return nil, fmt.Errorf("listing address prefix sets in ASG %q: %w", asgName, err)
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		c.log.Info("LIST AddressPrefixSets response", "page", pageNum, "statusCode", resp.StatusCode, "body", string(body))

		if err := checkStatusCode(resp.StatusCode, body, http.StatusOK); err != nil {
			return nil, err
		}

		var page AddressPrefixSetListResult
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, fmt.Errorf("decoding response: %w", err)
		}

		allResults = append(allResults, page.Value...)

		if page.NextLink == nil || *page.NextLink == "" {
			break
		}
		currentPath = *page.NextLink
	}

	c.log.Info("LIST AddressPrefixSets succeeded", "asgName", asgName, "count", len(allResults))
	return allResults, nil
}

func (c *AddressPrefixSetClient) resourcePath(asgName, prefixSetName string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/applicationSecurityGroups/%s/addressPrefixSets/%s",
		url.PathEscape(c.subscriptionID),
		url.PathEscape(c.resourceGroup),
		url.PathEscape(asgName),
		url.PathEscape(prefixSetName),
	)
}

func (c *AddressPrefixSetClient) getToken(ctx context.Context) (string, error) {
	token, err := c.credential.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{"https://management.azure.com/.default"},
	})
	if err != nil {
		return "", fmt.Errorf("acquiring token: %w", err)
	}
	return token.Token, nil
}

func (c *AddressPrefixSetClient) doRequest(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	var reqURL string
	if isAbsoluteURL(path) {
		reqURL = path
	} else {
		u, err := url.Parse(armEndpoint + path)
		if err != nil {
			return nil, fmt.Errorf("parsing URL: %w", err)
		}
		q := u.Query()
		q.Set("api-version", apiVersion2026)
		u.RawQuery = q.Encode()
		reqURL = u.String()
	}

	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		c.log.V(1).Info("REST request", "method", method, "url", reqURL, "body", string(data))
		bodyReader = bytes.NewReader(data)
	} else {
		c.log.V(1).Info("REST request", "method", method, "url", reqURL)
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	return http.DefaultClient.Do(req)
}

// pollLRO polls a long-running operation until completion using the Location header.
func (c *AddressPrefixSetClient) pollLRO(ctx context.Context, resp *http.Response) error {
	location := resp.Header.Get("Location")
	if location == "" {
		location = resp.Header.Get("Azure-AsyncOperation")
	}
	if location == "" {
		return nil
	}

	c.log.Info("polling LRO", "location", location)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}

		pollResp, err := c.doRequest(ctx, http.MethodGet, location, nil)
		if err != nil {
			return fmt.Errorf("polling LRO: %w", err)
		}
		pollResp.Body.Close()

		c.log.Info("LRO poll result", "statusCode", pollResp.StatusCode)

		if pollResp.StatusCode == http.StatusOK || pollResp.StatusCode == http.StatusNoContent {
			return nil
		}
		if pollResp.StatusCode != http.StatusAccepted {
			return fmt.Errorf("unexpected polling status: %d", pollResp.StatusCode)
		}
	}
}

func isAbsoluteURL(u string) bool {
	return len(u) > 8 && (u[:8] == "https://" || u[:7] == "http://")
}

func checkStatusCode(statusCode int, body []byte, allowed ...int) error {
	for _, code := range allowed {
		if statusCode == code {
			return nil
		}
	}
	return fmt.Errorf("unexpected status %d: %s", statusCode, string(body))
}
