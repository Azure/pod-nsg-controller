package main

import (
	"testing"

	"github.com/Azure/pod-nsg-controller/internal/azure"
)

func TestAddressPrefixSetAPIVersionLabel(t *testing.T) {
	t.Run("matches shared address prefix set API version", func(t *testing.T) {
		got := addressPrefixSetAPIVersionLabel()
		want := azure.AddressPrefixSetAPIVersion + addressPrefixSetVersionLabelSuffix
		if got != want {
			t.Errorf("addressPrefixSetAPIVersionLabel() = %q, want %q", got, want)
		}
	})
}
