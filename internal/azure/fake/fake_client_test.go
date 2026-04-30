package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/Azure/pod-nsg-controller/internal/azure"
)

// --- Fake client implements AddressPrefixSetAPI ---
func TestPhase4_FakeClient_ImplementsAddressPrefixSetAPI(t *testing.T) {
	client := NewClient()

	// Compile-time interface check
	var _ azure.AddressPrefixSetAPI = client

	// Runtime: verify basic CRUD cycle
	ctx := context.Background()

	// Get on missing resource should return ErrNotFound
	_, err := client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
	if !errors.Is(err, azure.ErrNotFound) {
		t.Errorf("expected ErrNotFound for missing resource, got: %v", err)
	}

	// Put should create
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.1/32"})
	if err != nil {
		t.Fatalf("Put should succeed: %v", err)
	}

	// Get should return the resource
	got, err := client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get should succeed after Put: %v", err)
	}
	if got == nil || got.Properties == nil || len(got.Properties.AddressPrefixes) != 1 {
		t.Errorf("expected 1 prefix, got %v", got)
	}

	// Delete should remove
	err = client.Delete(ctx, "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Delete should succeed: %v", err)
	}

	// Get after delete should return ErrNotFound
	_, err = client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
	if !errors.Is(err, azure.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got: %v", err)
	}
}

// --- Fake client simulates ETag 412 conflict ---
func TestPhase4_FakeClient_SimulatesETagConflict412(t *testing.T) {
	client := NewClient()
	client.Fail412Count = 2 // first 2 Puts return 412
	ctx := context.Background()

	// Pre-populate
	client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.1/32"})
	client.Fail412Count = 2
	client.fail412Calls = 0

	// First Put should fail with 412
	err := client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.2/32"})
	if err == nil {
		t.Fatal("expected 412 error on first Put")
	}
	if !azure.IsPreconditionFailed(err) {
		t.Errorf("expected PreconditionFailed error, got: %v", err)
	}

	// Second Put should also fail with 412
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.2/32"})
	if err == nil {
		t.Fatal("expected 412 error on second Put")
	}

	// Third Put should succeed
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.2/32"})
	if err != nil {
		t.Fatalf("expected third Put to succeed, got: %v", err)
	}
}

// --- Fake client: delete 404 is idempotent success ---
func TestPhase4_FakeClient_Delete404IsIdempotentSuccess(t *testing.T) {
	client := NewClient()
	ctx := context.Background()

	// Delete on a resource that doesn't exist should succeed (idempotent)
	err := client.Delete(ctx, "sub1", "rg1", "asg1", "nonexistent")
	if err != nil {
		t.Errorf("expected Delete on missing resource to succeed (idempotent), got: %v", err)
	}

	// Create then delete twice — verify Put actually stores data first
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.1/32"})
	if err != nil {
		t.Fatalf("Put should succeed: %v", err)
	}

	// Verify the resource exists after Put (this proves the fake works)
	got, err := client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get should succeed after Put: %v", err)
	}
	if got == nil || got.Properties == nil || len(got.Properties.AddressPrefixes) != 1 {
		t.Fatalf("expected 1 prefix after Put, got %v", got)
	}

	err = client.Delete(ctx, "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("first Delete should succeed: %v", err)
	}

	// After delete, Get should return ErrNotFound
	_, err = client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
	if !errors.Is(err, azure.ErrNotFound) {
		t.Errorf("expected ErrNotFound after delete, got: %v", err)
	}

	err = client.Delete(ctx, "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Errorf("second Delete should succeed (idempotent): %v", err)
	}
}

// --- Fake client: InjectError is applied before mutation ---
func TestPhase4_FakeClient_InjectError_BeforeMutation(t *testing.T) {
	client := NewClient()
	ctx := context.Background()

	// Pre-populate a resource
	err := client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.1/32"})
	if err != nil {
		t.Fatalf("initial Put failed: %v", err)
	}

	// Inject a 412 error on the next Put for this key
	injectKey := InjectKey{
		Operation:      OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  "ps1",
	}
	client.InjectError(injectKey, &azure.ARMStatusError{
		StatusCode: 412,
		ARMCode:    "PreconditionFailed",
		Message:    "injected conflict",
	}, 1)

	// Put should fail with injected error
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.2/32"})
	if err == nil {
		t.Fatal("expected injected 412 error, got nil")
	}
	if !azure.IsPreconditionFailed(err) {
		t.Errorf("expected PreconditionFailed error, got: %v", err)
	}

	// State should NOT have been mutated (error applied before mutation)
	got, err := client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get after failed Put returned error: %v", err)
	}
	if got.Properties == nil || len(got.Properties.AddressPrefixes) != 1 {
		t.Fatal("expected state unchanged after injected error")
	}
	if got.Properties.AddressPrefixes[0] != "10.0.0.1/32" {
		t.Errorf("expected IPs unchanged as [10.0.0.1/32], got %v", got.Properties.AddressPrefixes)
	}
}

// --- Fake client: InjectError consumed atomically per key ---
func TestPhase4_FakeClient_InjectError_ConsumedAtomicallyPerKey(t *testing.T) {
	client := NewClient()
	ctx := context.Background()

	// Inject 2 errors for key1 and 1 error for key2
	key1 := InjectKey{
		Operation:      OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  "ps1",
	}
	key2 := InjectKey{
		Operation:      OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  "ps2",
	}

	client.InjectError(key1, &azure.ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "conflict"}, 2)
	client.InjectError(key2, &azure.ARMStatusError{StatusCode: 500, ARMCode: "InternalServerError", Message: "server error"}, 1)

	// key1: first Put fails
	err := client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"1.1.1.1/32"})
	if err == nil {
		t.Error("key1 first Put should fail with injected error")
	}

	// key1: second Put also fails (2 errors injected)
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"1.1.1.1/32"})
	if err == nil {
		t.Error("key1 second Put should fail with second injected error")
	}

	// key1: third Put should succeed (errors exhausted)
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"1.1.1.1/32"})
	if err != nil {
		t.Errorf("key1 third Put should succeed after errors consumed, got: %v", err)
	}

	// key2: first Put fails
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps2", []string{"2.2.2.2/32"})
	if err == nil {
		t.Error("key2 first Put should fail with injected error")
	}

	// key2: second Put should succeed
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps2", []string{"2.2.2.2/32"})
	if err != nil {
		t.Errorf("key2 second Put should succeed after error consumed, got: %v", err)
	}
}

// --- Fake client: ETag version advances only on successful Put ---
func TestPhase4_FakeClient_ETagVersionAdvancesOnlyOnSuccessfulPut(t *testing.T) {
	client := NewClient()
	ctx := context.Background()

	// First successful Put
	err := client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.1/32"})
	if err != nil {
		t.Fatalf("first Put failed: %v", err)
	}

	got1, err := client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get after first Put failed: %v", err)
	}
	if got1.Etag == nil {
		t.Fatal("expected ETag after first Put, got nil")
	}
	etag1 := *got1.Etag

	// Inject a 412 error
	injectKey := InjectKey{
		Operation:      OperationPut,
		SubscriptionID: "sub1",
		ResourceGroup:  "rg1",
		ASGName:        "asg1",
		PrefixSetName:  "ps1",
	}
	client.InjectError(injectKey, &azure.ARMStatusError{StatusCode: 412, ARMCode: "PreconditionFailed", Message: "conflict"}, 1)

	// This Put should fail — ETag should NOT advance
	_ = client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.2/32"})

	got2, err := client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get after failed Put returned error: %v", err)
	}
	if got2.Etag == nil {
		t.Fatal("expected ETag to be preserved after failed Put")
	}
	if *got2.Etag != etag1 {
		t.Errorf("ETag should NOT advance after failed Put: was %q, now %q", etag1, *got2.Etag)
	}

	// IPs should be unchanged
	if got2.Properties == nil || len(got2.Properties.AddressPrefixes) != 1 || got2.Properties.AddressPrefixes[0] != "10.0.0.1/32" {
		t.Error("IPs should not change after failed Put")
	}

	// Second Put succeeds — ETag should advance
	err = client.Put(ctx, "sub1", "rg1", "asg1", "ps1", []string{"10.0.0.2/32"})
	if err != nil {
		t.Fatalf("second Put (after error consumed) failed: %v", err)
	}

	got3, err := client.Get(ctx, "sub1", "rg1", "asg1", "ps1")
	if err != nil {
		t.Fatalf("Get after successful Put failed: %v", err)
	}
	if got3.Etag == nil {
		t.Fatal("expected ETag after successful Put")
	}

	// Version/ETag must have advanced
	if *got3.Etag == etag1 {
		t.Error("ETag should advance after successful Put, but it stayed the same")
	}
}
