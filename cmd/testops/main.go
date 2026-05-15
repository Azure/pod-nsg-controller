package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Azure/pod-nsg-controller/internal/azure"
)

type testResult struct {
	num     int
	name    string
	status  string
	details string
}

func main() {
	subscriptionID := os.Getenv("AZURE_SUBSCRIPTION_ID")
	resourceGroup := os.Getenv("AZURE_RESOURCE_GROUP")
	asgName := os.Getenv("TEST_ASG_NAME")

	if subscriptionID == "" || resourceGroup == "" || asgName == "" {
		fmt.Println("Required env vars: AZURE_SUBSCRIPTION_ID, AZURE_RESOURCE_GROUP, TEST_ASG_NAME")
		os.Exit(1)
	}

	zapCfg := zap.NewProductionConfig()
	zapCfg.EncoderConfig.TimeKey = "timestamp"
	zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zapLog, err := zapCfg.Build()
	if err != nil {
		fmt.Printf("FATAL: cannot build logger: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = zapLog.Sync()
	}()
	logger := zapr.NewLogger(zapLog)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	results := make([]testResult, 0, 10)

	// --- SDK-based ASG operations ---
	asgClient, err := azure.NewASGClient(subscriptionID, resourceGroup, logger)
	if err != nil {
		fmt.Printf("FATAL: cannot create ASG client: %v\n", err)
		os.Exit(1)
	}

	// Test 1: GET ASG
	r := testResult{num: 1, name: "GET ASG"}
	asg, err := asgClient.Get(ctx, asgName)
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = fmt.Sprintf("id=%s, location=%s", deref(asg.ID), deref(asg.Location))
	}
	results = append(results, r)

	// Test 2: UpdateTags (PATCH)
	r = testResult{num: 2, name: "UpdateTags ASG (PATCH)"}
	_, err = asgClient.UpdateTags(ctx, asgName, map[string]*string{"test-controller": to.Ptr("true")})
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = "tags updated"
	}
	results = append(results, r)

	// Test 3: LIST ASGs (resource group)
	r = testResult{num: 3, name: "LIST ASGs (resource group)"}
	asgs, err := asgClient.List(ctx)
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = fmt.Sprintf("%d ASGs found", len(asgs))
	}
	results = append(results, r)

	// Test 4: LIST ALL ASGs (subscription)
	r = testResult{num: 4, name: "LIST ALL ASGs (subscription)"}
	allAsgs, err := asgClient.ListAll(ctx)
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = fmt.Sprintf("%d ASGs found", len(allAsgs))
	}
	results = append(results, r)

	// Test 5: CreateOrUpdate ASG (PUT)
	testASG := "asg-test-controller"
	r = testResult{num: 5, name: "CreateOrUpdate ASG (PUT)"}
	created, err := asgClient.CreateOrUpdate(ctx, testASG, "westus3", map[string]*string{"purpose": to.Ptr("test")})
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = fmt.Sprintf("created %s", deref(created.Name))
	}
	results = append(results, r)

	// --- REST-based AddressPrefixSet operations ---
	factory, err := azure.NewClientFactoryWithDefaultCredential(zapLog, nil)
	if err != nil {
		fmt.Printf("FATAL: cannot create AddressPrefixSet client factory: %v\n", err)
		os.Exit(1)
	}

	apsClient, err := factory.ForSubscription(subscriptionID)
	if err != nil {
		fmt.Printf("FATAL: cannot get AddressPrefixSet client: %v\n", err)
		os.Exit(1)
	}

	prefixSetName := "prefix-set-test"

	// Test 6: Put AddressPrefixSet
	r = testResult{num: 6, name: "PUT AddressPrefixSet"}
	err = apsClient.Put(ctx, subscriptionID, resourceGroup, testASG, prefixSetName, []string{"10.0.0.0/24", "10.0.1.0/24"})
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = "created"
	}
	results = append(results, r)

	// Test 7: GET AddressPrefixSet
	r = testResult{num: 7, name: "GET AddressPrefixSet"}
	aps2, err := apsClient.Get(ctx, subscriptionID, resourceGroup, testASG, prefixSetName)
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = fmt.Sprintf("id=%s", deref(aps2.ID))
	}
	results = append(results, r)

	// Test 8: LIST AddressPrefixSets
	r = testResult{num: 8, name: "LIST AddressPrefixSets"}
	apsList, err := apsClient.List(ctx, subscriptionID, resourceGroup, testASG)
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = fmt.Sprintf("%d prefix sets found", len(apsList))
	}
	results = append(results, r)

	// Test 9: DELETE AddressPrefixSet
	r = testResult{num: 9, name: "DELETE AddressPrefixSet"}
	err = apsClient.Delete(ctx, subscriptionID, resourceGroup, testASG, prefixSetName)
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = "deleted"
	}
	results = append(results, r)

	// Test 10: Delete ASG
	r = testResult{num: 10, name: "Delete ASG"}
	err = asgClient.Delete(ctx, testASG)
	if err != nil {
		r.status = "❌ FAIL"
		r.details = err.Error()
	} else {
		r.status = "✅ PASS"
		r.details = fmt.Sprintf("deleted %s", testASG)
	}
	results = append(results, r)

	// Print results
	fmt.Println("\n========================================")
	fmt.Println("  REST Operations Test Results")
	fmt.Println("  API Version: 2025-07-01 (AddressPrefixSets)")
	fmt.Println("========================================")
	passed, failed := 0, 0
	for _, r := range results {
		fmt.Printf("  %2d. %-35s %s\n", r.num, r.name, r.status)
		fmt.Printf("      %s\n", r.details)
		if r.status == "✅ PASS" {
			passed++
		} else {
			failed++
		}
	}
	fmt.Printf("\n  Total: %d passed, %d failed out of %d\n", passed, failed, len(results))
	fmt.Println("========================================")
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
