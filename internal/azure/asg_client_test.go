package azure

import (
	"testing"

	"go.uber.org/zap/zaptest"
)

func TestNewASGClient_LoggerHandling(t *testing.T) {
	const subscriptionID = "00000000-0000-0000-0000-000000000000"

	t.Run("nil logger defaults to nop", func(t *testing.T) {
		var (
			client *ASGClient
			err    error
		)

		defer func() {
			if r := recover(); r != nil {
				t.Errorf("NewASGClient panicked with nil logger: %v", r)
			}
			if err != nil {
				t.Errorf("NewASGClient returned error with nil logger: %v", err)
			}
			if client == nil {
				t.Error("NewASGClient returned nil client for nil logger")
			}
			if client != nil && client.log == nil {
				t.Error("ASGClient.log = nil, want non-nil logger")
			}
		}()

		client, err = NewASGClient(subscriptionID, "rg-test", nil)
	})

	t.Run("explicit logger remains usable", func(t *testing.T) {
		logger := zaptest.NewLogger(t)

		client, err := NewASGClient(subscriptionID, "rg-test", logger)
		if err != nil {
			t.Errorf("NewASGClient returned error with explicit logger: %v", err)
		}
		if client == nil {
			t.Error("NewASGClient returned nil client with explicit logger")
		}
		if client != nil && client.log == nil {
			t.Error("ASGClient.log = nil, want non-nil logger")
		}
	})
}
