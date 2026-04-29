package azure

import (
	"testing"

	"go.uber.org/zap/zaptest"
)

// --- T4.9: Client factory caches clients per subscription ---
func TestPhase4_T49_ClientFactoryCachesClientsPerSubscription(t *testing.T) {
	log := zaptest.NewLogger(t)
	factory := NewClientFactory(log)

	// First call for sub1
	client1, err := factory.ForSubscription("sub1")
	if err != nil {
		t.Fatalf("T4.9: ForSubscription(sub1) returned error: %v", err)
	}
	if client1 == nil {
		t.Fatal("T4.9: ForSubscription(sub1) returned nil client")
	}

	// Second call for sub1 should return same instance
	client1Again, err := factory.ForSubscription("sub1")
	if err != nil {
		t.Fatalf("T4.9: second ForSubscription(sub1) returned error: %v", err)
	}

	// Compare interfaces — same underlying object means caching works
	if client1 != client1Again {
		t.Error("T4.9: expected same client instance for same subscription, got different")
	}

	// Different subscription should get a different client
	client2, err := factory.ForSubscription("sub2")
	if err != nil {
		t.Fatalf("T4.9: ForSubscription(sub2) returned error: %v", err)
	}

	if client1 == client2 {
		t.Error("T4.9: expected different client instances for different subscriptions")
	}
}

// --- Additional: ForSubscription rejects empty subscription ID ---
func TestPhase4_ClientFactory_ForSubscriptionRejectsEmptySubscriptionID(t *testing.T) {
	log := zaptest.NewLogger(t)
	factory := NewClientFactory(log)

	_, err := factory.ForSubscription("")
	if err == nil {
		t.Error("expected error for empty subscription ID, got nil")
	}
}

// --- Additional: Factory uses DefaultAzureCredential when not injected ---
func TestPhase4_ClientFactory_UsesDefaultAzureCredentialWhenNotInjected(t *testing.T) {
	log := zaptest.NewLogger(t)

	// NewClientFactory (no credential) should lazily resolve DAC.
	// In test environment without Azure identity configured, ForSubscription
	// should still return a client (lazy credential resolution), but the
	// client should attempt to use DAC for real requests.
	factory := NewClientFactory(log)

	client, err := factory.ForSubscription("sub-dac-test")
	if err != nil {
		t.Fatalf("ForSubscription should not fail at client creation: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client from factory without injected credential")
	}

	// The returned client should be an *AddressPrefixSetClient with nil credential
	// (since factory has no credential injected). When DAC lazy resolution is
	// implemented, the credential should be non-nil.
	apsClient, ok := client.(*AddressPrefixSetClient)
	if !ok {
		t.Fatalf("expected *AddressPrefixSetClient, got %T", client)
	}

	// With lazy DAC, credential should be non-nil after resolution
	// Since this is a stub, credential will be nil — test MUST FAIL here
	if apsClient.credential == nil {
		t.Error("expected factory to provide DefaultAzureCredential, got nil (lazy DAC not yet implemented)")
	}
}

// --- Additional: Factory propagates ARM base URL to created clients ---
func TestPhase4_ClientFactory_PropagatesARMBaseURLToCreatedClient(t *testing.T) {
	log := zaptest.NewLogger(t)
	customURL := "https://custom-arm.example.com"

	factory := NewClientFactory(log, WithFactoryARMBaseURL(customURL))

	client, err := factory.ForSubscription("sub-url-test")
	if err != nil {
		t.Fatalf("ForSubscription returned error: %v", err)
	}
	if client == nil {
		t.Fatal("expected non-nil client")
	}

	// Cast to concrete type to inspect baseURL
	apsClient, ok := client.(*AddressPrefixSetClient)
	if !ok {
		t.Fatalf("expected *AddressPrefixSetClient, got %T", client)
	}

	if apsClient.baseURL != customURL {
		t.Errorf("expected baseURL %q, got %q", customURL, apsClient.baseURL)
	}
}
