package azure

import (
	"context"
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/go-logr/logr"
)

// ASGClient wraps the Azure SDK to manage Application Security Group operations.
type ASGClient struct {
	client        *armnetwork.ApplicationSecurityGroupsClient
	resourceGroup string
	log           logr.Logger
}

// NewASGClient creates a new ASGClient using DefaultAzureCredential.
func NewASGClient(subscriptionID, resourceGroup string, logger logr.Logger) (*ASGClient, error) {
	log := logger.WithName("ASGClient")
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("creating Azure credential: %w", err)
	}

	client, err := armnetwork.NewApplicationSecurityGroupsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("creating ASG client: %w", err)
	}

	log.Info("client initialized", "subscriptionID", subscriptionID, "resourceGroup", resourceGroup)

	return &ASGClient{
		client:        client,
		resourceGroup: resourceGroup,
		log:           log,
	}, nil
}

// Get returns the Application Security Group with the given name.
func (c *ASGClient) Get(ctx context.Context, asgName string) (*armnetwork.ApplicationSecurityGroup, error) {
	c.log.Info("GET ASG", "resourceGroup", c.resourceGroup, "asgName", asgName)

	resp, err := c.client.Get(ctx, c.resourceGroup, asgName, nil)
	if err != nil {
		c.log.Error(err, "GET ASG failed", "asgName", asgName)
		return nil, fmt.Errorf("getting ASG %q: %w", asgName, err)
	}

	c.log.Info("GET ASG succeeded", "asgName", asgName,
		"id", ptrVal(resp.ID), "location", ptrVal(resp.Location),
		"provisioningState", provisioningStateVal(resp.Properties.ProvisioningState))
	return &resp.ApplicationSecurityGroup, nil
}

// CreateOrUpdate creates or updates an Application Security Group.
func (c *ASGClient) CreateOrUpdate(ctx context.Context, asgName, location string, tags map[string]*string) (*armnetwork.ApplicationSecurityGroup, error) {
	c.log.Info("PUT ASG", "resourceGroup", c.resourceGroup, "asgName", asgName,
		"location", location, "tagCount", len(tags))

	params := armnetwork.ApplicationSecurityGroup{
		Location: to.Ptr(location),
		Tags:     tags,
	}

	poller, err := c.client.BeginCreateOrUpdate(ctx, c.resourceGroup, asgName, params, nil)
	if err != nil {
		c.log.Error(err, "PUT ASG failed to start", "asgName", asgName)
		return nil, fmt.Errorf("starting create/update for ASG %q: %w", asgName, err)
	}

	c.log.Info("PUT ASG polling for completion", "asgName", asgName)
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		c.log.Error(err, "PUT ASG polling failed", "asgName", asgName)
		return nil, fmt.Errorf("creating/updating ASG %q: %w", asgName, err)
	}

	c.log.Info("PUT ASG succeeded", "asgName", asgName,
		"id", ptrVal(resp.ID), "provisioningState", provisioningStateVal(resp.Properties.ProvisioningState))
	return &resp.ApplicationSecurityGroup, nil
}

// Delete removes an Application Security Group.
func (c *ASGClient) Delete(ctx context.Context, asgName string) error {
	c.log.Info("DELETE ASG", "resourceGroup", c.resourceGroup, "asgName", asgName)

	poller, err := c.client.BeginDelete(ctx, c.resourceGroup, asgName, nil)
	if err != nil {
		c.log.Error(err, "DELETE ASG failed to start", "asgName", asgName)
		return fmt.Errorf("starting delete for ASG %q: %w", asgName, err)
	}

	c.log.Info("DELETE ASG polling for completion", "asgName", asgName)
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		c.log.Error(err, "DELETE ASG polling failed", "asgName", asgName)
		return fmt.Errorf("deleting ASG %q: %w", asgName, err)
	}

	c.log.Info("DELETE ASG succeeded", "asgName", asgName)
	return nil
}

// UpdateTags patches tags on an Application Security Group without modifying other properties.
func (c *ASGClient) UpdateTags(ctx context.Context, asgName string, tags map[string]*string) (*armnetwork.ApplicationSecurityGroup, error) {
	c.log.Info("PATCH ASG tags", "resourceGroup", c.resourceGroup, "asgName", asgName,
		"tagCount", len(tags))

	params := armnetwork.TagsObject{
		Tags: tags,
	}

	resp, err := c.client.UpdateTags(ctx, c.resourceGroup, asgName, params, nil)
	if err != nil {
		c.log.Error(err, "PATCH ASG tags failed", "asgName", asgName)
		return nil, fmt.Errorf("updating tags on ASG %q: %w", asgName, err)
	}

	c.log.Info("PATCH ASG tags succeeded", "asgName", asgName,
		"id", ptrVal(resp.ID), "provisioningState", provisioningStateVal(resp.Properties.ProvisioningState))
	return &resp.ApplicationSecurityGroup, nil
}

// List returns all Application Security Groups in the resource group.
func (c *ASGClient) List(ctx context.Context) ([]*armnetwork.ApplicationSecurityGroup, error) {
	c.log.Info("LIST ASGs", "resourceGroup", c.resourceGroup)

	var asgs []*armnetwork.ApplicationSecurityGroup
	pager := c.client.NewListPager(c.resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			c.log.Error(err, "LIST ASGs failed")
			return nil, fmt.Errorf("listing ASGs: %w", err)
		}
		asgs = append(asgs, page.Value...)
	}

	c.log.Info("LIST ASGs succeeded", "count", len(asgs))
	return asgs, nil
}

// ListAll returns all Application Security Groups across the entire subscription.
func (c *ASGClient) ListAll(ctx context.Context) ([]*armnetwork.ApplicationSecurityGroup, error) {
	c.log.Info("LIST ALL ASGs in subscription")

	var asgs []*armnetwork.ApplicationSecurityGroup
	pager := c.client.NewListAllPager(nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			c.log.Error(err, "LIST ALL ASGs failed")
			return nil, fmt.Errorf("listing all ASGs in subscription: %w", err)
		}
		asgs = append(asgs, page.Value...)
	}

	c.log.Info("LIST ALL ASGs succeeded", "count", len(asgs))
	return asgs, nil
}

func ptrVal(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func provisioningStateVal(s *armnetwork.ProvisioningState) string {
	if s == nil {
		return "<nil>"
	}
	return string(*s)
}
