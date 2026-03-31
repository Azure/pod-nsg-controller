package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
)

// ASGClient wraps the Azure SDK to manage Application Security Group operations.
type ASGClient struct {
	client        *armnetwork.ApplicationSecurityGroupsClient
	resourceGroup string
}

// NewASGClient creates a new ASGClient using DefaultAzureCredential.
func NewASGClient(subscriptionID, resourceGroup string) (*ASGClient, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("creating Azure credential: %w", err)
	}

	client, err := armnetwork.NewApplicationSecurityGroupsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating ASG client: %w", err)
	}

	return &ASGClient{
		client:        client,
		resourceGroup: resourceGroup,
	}, nil
}

// Get returns the Application Security Group with the given name.
func (c *ASGClient) Get(ctx context.Context, asgName string) (*armnetwork.ApplicationSecurityGroup, error) {
	resp, err := c.client.Get(ctx, c.resourceGroup, asgName, nil)
	if err != nil {
		return nil, fmt.Errorf("getting ASG %q: %w", asgName, err)
	}
	return &resp.ApplicationSecurityGroup, nil
}

// List returns all Application Security Groups in the resource group.
func (c *ASGClient) List(ctx context.Context) ([]*armnetwork.ApplicationSecurityGroup, error) {
	var asgs []*armnetwork.ApplicationSecurityGroup
	pager := c.client.NewListPager(c.resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing ASGs: %w", err)
		}
		asgs = append(asgs, page.Value...)
	}
	return asgs, nil
}
