package azure

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/pkg/errors"
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
		return nil, errors.Wrap(err, "creating Azure credential")
	}

	client, err := armnetwork.NewInterfacesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, errors.Wrap(err, "creating NIC client")
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
		return nil, errors.Wrapf(err, "getting NIC %q", nicName)
	}
	return &resp.Interface, nil
}

// UpdateASGs updates the ASG associations on a network interface.
// It replaces the NIC's current ASG list on the specified IP configuration.
func (c *NICClient) UpdateASGs(ctx context.Context, nicName string, nic *armnetwork.Interface) error {
	poller, err := c.client.BeginCreateOrUpdate(ctx, c.resourceGroup, nicName, *nic, nil)
	if err != nil {
		return errors.Wrapf(err, "starting NIC update for %q", nicName)
	}

	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return errors.Wrapf(err, "updating NIC %q", nicName)
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
			return nil, errors.Wrap(err, "listing NICs")
		}
		nics = append(nics, page.Value...)
	}
	return nics, nil
}
