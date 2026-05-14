package azure

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"go.uber.org/zap/zaptest"
)

// TestAddressPrefixSetClient_Get_UsesHeaderETagOverBodyETag verifies that
// Get extracts ETag from the response header with precedence over the body etag.
func TestAddressPrefixSetClient_Get_UsesHeaderETagOverBodyETag(t *testing.T) {
	headerETag := `"header-etag-value"`
	bodyETag := `"body-etag-value"`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/oauth2/") {
			// token endpoint
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"access_token": "fake-token",
				"token_type":   "Bearer",
				"expires_in":   3600,
			})
			return
		}
		w.Header().Set("ETag", headerETag)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			"name": "ps1",
			"etag": bodyETag,
			"properties": map[string]interface{}{
				"addressPrefixSet": []string{"10.0.0.1/32"},
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	result, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if result == nil {
		t.Fatal("Get returned nil result")
	}
	if result.Etag == nil || *result.Etag != headerETag {
		got := "<nil>"
		if result.Etag != nil {
			got = *result.Etag
		}
		t.Errorf("expected ETag from header %q, got %q", headerETag, got)
	}
}

// TestAddressPrefixSetClient_Get_MissingETagReturnsErrMissingETag verifies
// that Get returns ErrMissingETag when neither header nor body has an ETag.
func TestAddressPrefixSetClient_Get_MissingETagReturnsErrMissingETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No ETag header, no etag in body
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			"name": "ps1",
			"properties": map[string]interface{}{
				"addressPrefixSet": []string{"10.0.0.1/32"},
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	_, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err == nil {
		t.Fatal("expected ErrMissingETag, got nil error")
	}
	if !strings.Contains(err.Error(), "missing etag") {
		t.Errorf("expected ErrMissingETag, got: %v", err)
	}
}

// TestAddressPrefixSetClient_Get_EmptyListEnvelopeReturnsNotFound verifies
// that Get returns ErrNotFound when the ARM API returns HTTP 200 with a
// list envelope (e.g. {"value":[]}) instead of a 404 for a missing resource.
func TestAddressPrefixSetClient_Get_EmptyListEnvelopeReturnsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// ARM returns list envelope instead of 404 for non-existent resource
		w.Write([]byte(`{"value":[]}`))
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	_, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got: %v", err)
	}
}

// TestAddressPrefixSetClient_Get_ListEnvelopeWithOneResource verifies that
// Get correctly extracts an AddressPrefixSet from a list envelope {"value":[...]}
// containing a single resource, which is the production POC behavior where
// single-resource GET returns a list envelope instead of a bare object.
func TestAddressPrefixSetClient_Get_ListEnvelopeWithOneResource(t *testing.T) {
	expectedID := "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1"
	expectedName := "ps1"
	expectedETag := `"list-envelope-etag"`
	expectedIPs := []string{"10.0.0.1/32", "10.0.0.2/32"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", expectedETag)
		w.Header().Set("Content-Type", "application/json")
		// ARM returns list envelope for single-resource GET
		json.NewEncoder(w).Encode(map[string]interface{}{
			"value": []map[string]interface{}{
				{
					"id":   expectedID,
					"name": expectedName,
					"etag": `"body-etag-ignored"`,
					"type": "Microsoft.Network/applicationSecurityGroups/addressPrefixSets",
					"properties": map[string]interface{}{
						"addressPrefixSet":  expectedIPs,
						"provisioningState": "Succeeded",
					},
				},
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	result, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get returned unexpected error: %v", err)
	}

	if result.ID == nil || *result.ID != expectedID {
		t.Errorf("expected ID %q, got %v", expectedID, result.ID)
	}
	if result.Name == nil || *result.Name != expectedName {
		t.Errorf("expected Name %q, got %v", expectedName, result.Name)
	}
	// Header ETag takes precedence over body etag
	if result.Etag == nil || *result.Etag != expectedETag {
		t.Errorf("expected ETag %q, got %v", expectedETag, result.Etag)
	}
	if result.Type == nil || *result.Type != "Microsoft.Network/applicationSecurityGroups/addressPrefixSets" {
		t.Errorf("expected Type field, got %v", result.Type)
	}
	if result.Properties == nil {
		t.Fatal("expected non-nil Properties")
	}
	if len(result.Properties.AddressPrefixes) != len(expectedIPs) {
		t.Fatalf("expected %d address prefixes, got %d", len(expectedIPs), len(result.Properties.AddressPrefixes))
	}
	for i, ip := range expectedIPs {
		if result.Properties.AddressPrefixes[i] != ip {
			t.Errorf("expected prefix[%d] = %q, got %q", i, ip, result.Properties.AddressPrefixes[i])
		}
	}
	if result.Properties.ProvisioningState == nil || *result.Properties.ProvisioningState != "Succeeded" {
		t.Errorf("expected ProvisioningState 'Succeeded', got %v", result.Properties.ProvisioningState)
	}
}

// TestAddressPrefixSetClient_Put_UsesIfMatchForExisting verifies that
// Put sends If-Match header with ETag for resources confirmed to exist.
func TestAddressPrefixSetClient_Put_UsesIfMatchForExisting(t *testing.T) {
	var capturedIfMatch string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			capturedIfMatch = r.Header.Get("If-Match")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
				"name": "ps1",
			})
			return
		}
		// GET response for ETag cache population
		w.Header().Set("ETag", `"existing-etag"`)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			"name": "ps1",
			"etag": `"existing-etag"`,
			"properties": map[string]interface{}{
				"addressPrefixSet": []string{},
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	err := client.Put(context.Background(), "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.1/32"})
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	expectedIfMatch := `"existing-etag"`
	if capturedIfMatch != expectedIfMatch {
		t.Errorf("expected If-Match header %q, got %q", expectedIfMatch, capturedIfMatch)
	}
}

// TestAddressPrefixSetClient_Put_UsesIfNoneMatchForConfirmedNotFound verifies
// that Put sends If-None-Match: * when the resource is confirmed not found.
func TestAddressPrefixSetClient_Put_UsesIfNoneMatchForConfirmedNotFound(t *testing.T) {
	var capturedIfNoneMatch string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "ResourceNotFound",
					"message": "not found",
				},
			})
			return
		}
		if r.Method == http.MethodPut {
			capturedIfNoneMatch = r.Header.Get("If-None-Match")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
				"name": "ps1",
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	err := client.Put(context.Background(), "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.1/32"})
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	if capturedIfNoneMatch != "*" {
		t.Errorf("expected If-None-Match header %q, got %q", "*", capturedIfNoneMatch)
	}
}

// TestAddressPrefixSetClient_Put_UsesInjectedHTTPClientForAllRequests verifies
// that all HTTP requests go through the injected httpClient, not http.DefaultClient.
func TestAddressPrefixSetClient_Put_UsesInjectedHTTPClientForAllRequests(t *testing.T) {
	requestCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{
					"code":    "ResourceNotFound",
					"message": "not found",
				},
			})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	_ = client.Put(context.Background(), "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.1/32"})

	if requestCount == 0 {
		t.Error("expected requests through injected httpClient, but none were made")
	}
}

// TestAddressPrefixSetClient_Put_PollsLROBeforeSuccess verifies that
// Put polls the LRO endpoint on 201/202 before reporting success.
func TestAddressPrefixSetClient_Put_PollsLROBeforeSuccess(t *testing.T) {
	lroPolled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "operationResults") {
			lroPolled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodPut {
			w.Header().Set("Location", "/subscriptions/sub1/providers/Microsoft.Network/locations/eastus/operationResults/op1")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			})
			return
		}
		// GET for ETag / result
		w.Header().Set("ETag", `"etag1"`)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			"name": "ps1",
			"etag": `"etag1"`,
			"properties": map[string]interface{}{
				"addressPrefixSet": []string{"10.0.0.1/32"},
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	err := client.Put(context.Background(), "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.1/32"})
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	if !lroPolled {
		t.Error("expected LRO polling on 201, but no poll request was made")
	}
}

// TestAddressPrefixSetClient_Delete_PollsLROOn202BeforeSuccess verifies that
// Delete polls the LRO endpoint on 202 before reporting success.
func TestAddressPrefixSetClient_Delete_PollsLROOn202BeforeSuccess(t *testing.T) {
	lroPolled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "operationResults") {
			lroPolled = true
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodDelete {
			w.Header().Set("Location", "/subscriptions/sub1/providers/Microsoft.Network/locations/eastus/operationResults/op1")
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	err := client.Delete(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if !lroPolled {
		t.Error("expected LRO polling on 202, but no poll request was made")
	}
}

func TestAddressPrefixSetClient_Delete_NotFoundIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Fatalf("expected DELETE request, got %s", r.Method)
		}
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "ResourceNotFound",
				"message": "not found",
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	err := client.Delete(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Delete should treat 404 as success, got: %v", err)
	}
}

// TestAddressPrefixSetClient_Put_EmptyIPListSerializesAddressPrefixesEmptyArray verifies
// T4.10: Put with empty IP list serializes as "addressPrefixSet":[] not omitted.
func TestAddressPrefixSetClient_Put_EmptyIPListSerializesAddressPrefixesEmptyArray(t *testing.T) {
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			capturedBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			})
			return
		}
		// GET - return not found so Put takes the create path
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "ResourceNotFound",
				"message": "not found",
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	err := client.Put(context.Background(), "sub1", "rg1", "asg1", "ps1", []string{})
	if err != nil {
		t.Fatalf("Put returned error: %v", err)
	}

	if len(capturedBody) == 0 {
		t.Fatal("expected PUT body to be captured, got empty")
	}

	bodyStr := string(capturedBody)
	// The body must contain "addressPrefixSet":[] and NOT omit the field
	if !strings.Contains(bodyStr, `"addressPrefixSet":[]`) &&
		!strings.Contains(bodyStr, `"addressPrefixSet": []`) {
		t.Errorf("expected PUT body to contain addressPrefixes as empty array, got: %s", bodyStr)
	}
}

// TestAddressPrefixSetClient_Get_CopiesResolvedETagOntoResult verifies that
// Get copies the resolved ETag value onto the returned AddressPrefixSet.Etag field.
func TestAddressPrefixSetClient_Get_CopiesResolvedETagOntoResult(t *testing.T) {
	expectedETag := `"resolved-etag-v1"`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", expectedETag)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			"name": "ps1",
			"properties": map[string]interface{}{
				"addressPrefixSet": []string{"10.0.0.1/32"},
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	result, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}
	if result == nil {
		t.Fatal("Get returned nil result")
	}
	if result.Etag == nil {
		t.Fatal("expected Etag to be set on result, got nil")
	}
	if *result.Etag != expectedETag {
		t.Errorf("expected Etag %q on result, got %q", expectedETag, *result.Etag)
	}
}

// TestAddressPrefixSetClient_NilCredentialSkipsAuthorizationAndUsesInjectedHTTPClient verifies
// that when credential is nil, no Authorization header is sent and the injected httpClient is used.
func TestAddressPrefixSetClient_NilCredentialSkipsAuthorizationAndUsesInjectedHTTPClient(t *testing.T) {
	var capturedAuthHeader string
	requestCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		capturedAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("ETag", `"etag-1"`)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			"name": "ps1",
			"etag": `"etag-1"`,
			"properties": map[string]interface{}{
				"addressPrefixSet": []string{"10.0.0.1/32"},
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	// nil credential = test mode, should skip Authorization header
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	_, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get returned error: %v", err)
	}

	if requestCount == 0 {
		t.Error("expected requests through injected httpClient, but none were made")
	}

	if capturedAuthHeader != "" {
		t.Errorf("expected no Authorization header with nil credential, got %q", capturedAuthHeader)
	}
}

// TestAddressPrefixSetClient_AllHTTPFailuresReturnARMStatusError verifies
// that all non-2xx responses return *ARMStatusError.
func TestAddressPrefixSetClient_AllHTTPFailuresReturnARMStatusError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		armCode    string
	}{
		{"400 BadRequest", http.StatusBadRequest, "BadRequest"},
		{"403 Forbidden", http.StatusForbidden, "AuthorizationFailed"},
		{"404 NotFound", http.StatusNotFound, "ResourceNotFound"},
		{"409 Conflict", http.StatusConflict, "Conflict"},
		{"412 PreconditionFailed", http.StatusPreconditionFailed, "PreconditionFailed"},
		{"429 TooManyRequests", http.StatusTooManyRequests, "TooManyRequests"},
		{"500 InternalServerError", http.StatusInternalServerError, "InternalServerError"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.statusCode)
				json.NewEncoder(w).Encode(map[string]interface{}{
					"error": map[string]interface{}{
						"code":    tc.armCode,
						"message": "test error",
					},
				})
			}))
			defer srv.Close()

			log := zaptest.NewLogger(t)
			client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

			_, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
			if err == nil {
				t.Fatalf("expected error for status %d, got nil", tc.statusCode)
			}

			var armErr *ARMStatusError
			if !errors.As(err, &armErr) {
				t.Errorf("expected *ARMStatusError for status %d, got %T: %v", tc.statusCode, err, err)
			}
			if armErr != nil && armErr.StatusCode != tc.statusCode {
				t.Errorf("expected ARMStatusError.StatusCode=%d, got %d", tc.statusCode, armErr.StatusCode)
			}
		})
	}
}

// TestAddressPrefixSetClient_RetriesTransientThenSucceeds verifies that doRequest
// retries on 503 and succeeds when the server recovers within the retry limit.
func TestAddressPrefixSetClient_RetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			// First 2 calls return 503
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"error": map[string]interface{}{"code": "ServiceUnavailable", "message": "retry later"},
			})
			return
		}
		// 3rd call succeeds
		w.Header().Set("ETag", `"etag-ok"`)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			"name": "ps1",
			"properties": map[string]interface{}{
				"addressPrefixSet": []string{"10.0.0.1/32"},
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	result, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("expected success after retries, got error: %v", err)
	}
	if result == nil || result.Etag == nil || *result.Etag != `"etag-ok"` {
		t.Fatalf("unexpected result: %+v", result)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("expected 3 total requests (2 retries + 1 success), got %d", got)
	}
}

// TestAddressPrefixSetClient_RetriesExhaustedReturnsLastError verifies that when all
// retries are exhausted, the final retryable response is returned as an ARMStatusError.
func TestAddressPrefixSetClient_RetriesExhaustedReturnsLastError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{"code": "TooManyRequests", "message": "throttled"},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, nil, srv.Client(), WithARMBaseURL(srv.URL))

	_, err := client.Get(context.Background(), "sub1", "rg1", "asg1", "ps1")
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}

	var armErr *ARMStatusError
	if !errors.As(err, &armErr) {
		t.Fatalf("expected *ARMStatusError, got %T: %v", err, err)
	}
	if armErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", armErr.StatusCode)
	}

	// 1 initial + 3 retries = 4 total
	if got := calls.Load(); got != 4 {
		t.Errorf("expected 4 total requests, got %d", got)
	}
}

// fakeCredential is a test credential that counts GetToken calls.
type fakeCredential struct {
	calls     atomic.Int64
	token     string
	expiresOn time.Time
}

func (f *fakeCredential) GetToken(_ context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.calls.Add(1)
	return azcore.AccessToken{Token: f.token, ExpiresOn: f.expiresOn}, nil
}

// TestAddressPrefixSetClient_AcquireToken_CachesUntilExpiry verifies that
// acquireToken caches the token and only calls GetToken again when the
// cached token is within the refresh margin of expiry.
func TestAddressPrefixSetClient_AcquireToken_CachesUntilExpiry(t *testing.T) {
	cred := &fakeCredential{
		token:     "cached-token",
		expiresOn: time.Now().Add(time.Hour), // expires far in the future
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"etag-1"`)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   "/subscriptions/sub1/resourceGroups/rg1/providers/Microsoft.Network/applicationSecurityGroups/asg1/addressPrefixSets/ps1",
			"name": "ps1",
			"properties": map[string]interface{}{
				"addressPrefixSet": []string{"10.0.0.1/32"},
			},
		})
	}))
	defer srv.Close()

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, cred, srv.Client(), WithARMBaseURL(srv.URL))

	ctx := context.Background()

	// Multiple Get calls should reuse the cached token.
	for i := 0; i < 5; i++ {
		_, err := client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
		if err != nil {
			t.Fatalf("Get[%d] returned error: %v", i, err)
		}
	}

	// GetToken should have been called only once.
	if got := cred.calls.Load(); got != 1 {
		t.Errorf("expected 1 GetToken call (cached), got %d", got)
	}
}

// TestAddressPrefixSetClient_AcquireToken_RefreshesExpiredToken verifies that
// acquireToken refreshes the token when it is within the refresh margin.
func TestAddressPrefixSetClient_AcquireToken_RefreshesExpiredToken(t *testing.T) {
	cred := &fakeCredential{
		token:     "refreshed-token",
		expiresOn: time.Now().Add(time.Hour),
	}

	log := zaptest.NewLogger(t)
	client := NewAddressPrefixSetClient(log, cred, nil, WithARMBaseURL("http://unused"))

	// Seed the cache with an about-to-expire token.
	client.cachedToken = "old-token"
	client.tokenExpiresOn = time.Now().Add(2 * time.Minute) // within 5-min margin

	ctx := context.Background()
	token, err := client.acquireToken(ctx)
	if err != nil {
		t.Fatalf("acquireToken returned error: %v", err)
	}

	if token != "refreshed-token" {
		t.Errorf("expected refreshed token, got %q", token)
	}
	if got := cred.calls.Load(); got != 1 {
		t.Errorf("expected 1 GetToken call for refresh, got %d", got)
	}
}
