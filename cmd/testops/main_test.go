package main

import (
	"testing"

	"github.com/Azure/pod-nsg-controller/internal/azure"
	"go.uber.org/zap/zaptest"
)

func TestAddressPrefixSetAPIVersionLabel(t *testing.T) {
	t.Run("matches shared address prefix set API version", func(t *testing.T) {
		logger := zaptest.NewLogger(t)
		logger.Debug("verifying address prefix set API version label")

		got := addressPrefixSetAPIVersionLabel()
		want := azure.AddressPrefixSetAPIVersion + " (AddressPrefixSets)"
		if got != want {
			t.Errorf("addressPrefixSetAPIVersionLabel() = %q, want %q", got, want)
		}
	})
}
