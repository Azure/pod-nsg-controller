package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
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

	// Bootstrap a minimal logger for pre-init fatals.
	bootstrap, _ := zap.NewProduction()

	if subscriptionID == "" || resourceGroup == "" || asgName == "" {
		bootstrap.Fatal("Required env vars not set",
			zap.Strings("required", []string{"AZURE_SUBSCRIPTION_ID", "AZURE_RESOURCE_GROUP", "TEST_ASG_NAME"}))
	}

	zapCfg := zap.NewProductionConfig()
	zapCfg.EncoderConfig.TimeKey = "timestamp"
	zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zapLog, err := zapCfg.Build()
	if err != nil {
		bootstrap.Fatal("cannot build logger", zap.Error(err))
	}
	defer func() {
		_ = zapLog.Sync()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	results := make([]testResult, 0, 10)

	// --- SDK-based ASG operations ---
	asgClient, err := azure.NewASGClient(subscriptionID, resourceGroup, zapLog)
	if err != nil {
		zapLog.Fatal("cannot create ASG client", zap.Error(err))
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
		zapLog.Fatal("cannot create AddressPrefixSet client factory", zap.Error(err))
	}

	apsClient, err := factory.ForSubscription(subscriptionID)
	if err != nil {
		zapLog.Fatal("cannot get AddressPrefixSet client", zap.Error(err))
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
	zapLog.Info("========================================")
	zapLog.Info("REST Operations Test Results")
	zapLog.Info("API Version: 2025-07-01 (AddressPrefixSets)")
	zapLog.Info("========================================")
	passed, failed := 0, 0
	for _, r := range results {
		zapLog.Info("test result",
			zap.Int("num", r.num),
			zap.String("name", r.name),
			zap.String("status", r.status),
			zap.String("details", r.details),
		)
		if r.status == "✅ PASS" {
			passed++
		} else {
			failed++
		}
	}
	zapLog.Info("REST Operations Test Results",
		zap.String("apiVersion", "2026-01-01 (AddressPrefixSets)"),
		zap.Int("passed", passed),
		zap.Int("failed", failed),
		zap.Int("total", len(results)),
	)
	if failed > 0 {
		_ = zapLog.Sync()
		os.Exit(1)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
