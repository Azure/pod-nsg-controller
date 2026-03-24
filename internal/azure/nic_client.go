package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

// NICClient wraps the Azure SDK to manage Network Interface operations.
type NICClient struct {
	client        *armnetwork.InterfacesClient
	resourceGroup string
}

// NewNICClient creates a new NICClient using DefaultAzureCredential.
func NewNICClient(subscriptionID, resourceGroup string) (*NICClient, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("creating Azure credential: %w", err)
	}

	client, err := armnetwork.NewInterfacesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating NIC client: %w", err)
	}

	return &NICClient{
		client:        client,
		resourceGroup: resourceGroup,
	}, nil
}

// Get returns the network interface with the given name.
func (c *NICClient) Get(ctx context.Context, nicName string) (*armnetwork.Interface, error) {
	resp, err := c.client.Get(ctx, c.resourceGroup, nicName, nil)
	if err != nil {
		return nil, fmt.Errorf("getting NIC %q: %w", nicName, err)
	}
	return &resp.Interface, nil
}

// UpdateASGs updates the ASG associations on a network interface.
// It replaces the NIC's current ASG list on the specified IP configuration.
func (c *NICClient) UpdateASGs(ctx context.Context, nicName string, nic *armnetwork.Interface) error {
	poller, err := c.client.BeginCreateOrUpdate(ctx, c.resourceGroup, nicName, *nic, nil)
	if err != nil {
		return fmt.Errorf("starting NIC update for %q: %w", nicName, err)
	}

	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("updating NIC %q: %w", nicName, err)
	}

	return nil
}

// ListByResourceGroup returns all network interfaces in the resource group.
func (c *NICClient) ListByResourceGroup(ctx context.Context) ([]*armnetwork.Interface, error) {
	var nics []*armnetwork.Interface
	pager := c.client.NewListPager(c.resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing NICs: %w", err)
		}
		nics = append(nics, page.Value...)
	}
	return nics, nil
}
