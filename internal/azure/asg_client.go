package azure

import (
	"context"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v6"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

// ASGClient wraps the Azure SDK to manage Application Security Group operations.
type ASGClient struct {
	client        *armnetwork.ApplicationSecurityGroupsClient
	resourceGroup string
	log           *zap.Logger
}

// NewASGClient creates a new ASGClient using DefaultAzureCredential.
func NewASGClient(subscriptionID, resourceGroup string, logger *zap.Logger) (*ASGClient, error) {
	log := logger.Named("ASGClient")
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, errors.Wrap(err, "creating Azure credential")
	}

	client, err := armnetwork.NewApplicationSecurityGroupsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, errors.Wrap(err, "creating ASG client")
	}

	log.Info("client initialized", zap.String("subscriptionID", subscriptionID), zap.String("resourceGroup", resourceGroup))

	return &ASGClient{
		client:        client,
		resourceGroup: resourceGroup,
		log:           log,
	}, nil
}

// Get returns the Application Security Group with the given name.
func (c *ASGClient) Get(ctx context.Context, asgName string) (*armnetwork.ApplicationSecurityGroup, error) {
	c.log.Info("GET ASG", zap.String("resourceGroup", c.resourceGroup), zap.String("asgName", asgName))

	resp, err := c.client.Get(ctx, c.resourceGroup, asgName, nil)
	if err != nil {
		c.log.Error("GET ASG failed", zap.String("asgName", asgName), zap.Error(err))
		return nil, errors.Wrapf(err, "getting ASG %q", asgName)
	}

	c.log.Info("GET ASG succeeded", zap.String("asgName", asgName),
		zap.String("id", ptrVal(resp.ID)), zap.String("location", ptrVal(resp.Location)),
		zap.String("provisioningState", provisioningStateVal(resp.Properties.ProvisioningState)))
	return &resp.ApplicationSecurityGroup, nil
}

// CreateOrUpdate creates or updates an Application Security Group.
func (c *ASGClient) CreateOrUpdate(ctx context.Context, asgName, location string, tags map[string]*string) (*armnetwork.ApplicationSecurityGroup, error) {
	c.log.Info("PUT ASG", zap.String("resourceGroup", c.resourceGroup), zap.String("asgName", asgName),
		zap.String("location", location), zap.Int("tagCount", len(tags)))

	params := armnetwork.ApplicationSecurityGroup{
		Location: to.Ptr(location),
		Tags:     tags,
	}

	poller, err := c.client.BeginCreateOrUpdate(ctx, c.resourceGroup, asgName, params, nil)
	if err != nil {
		c.log.Error("PUT ASG failed to start", zap.String("asgName", asgName), zap.Error(err))
		return nil, errors.Wrapf(err, "starting create/update for ASG %q", asgName)
	}

	c.log.Info("PUT ASG polling for completion", zap.String("asgName", asgName))
	resp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		c.log.Error("PUT ASG polling failed", zap.String("asgName", asgName), zap.Error(err))
		return nil, errors.Wrapf(err, "creating/updating ASG %q", asgName)
	}

	c.log.Info("PUT ASG succeeded", zap.String("asgName", asgName),
		zap.String("id", ptrVal(resp.ID)), zap.String("provisioningState", provisioningStateVal(resp.Properties.ProvisioningState)))
	return &resp.ApplicationSecurityGroup, nil
}

// Delete removes an Application Security Group.
func (c *ASGClient) Delete(ctx context.Context, asgName string) error {
	c.log.Info("DELETE ASG", zap.String("resourceGroup", c.resourceGroup), zap.String("asgName", asgName))

	poller, err := c.client.BeginDelete(ctx, c.resourceGroup, asgName, nil)
	if err != nil {
		c.log.Error("DELETE ASG failed to start", zap.String("asgName", asgName), zap.Error(err))
		return errors.Wrapf(err, "starting delete for ASG %q", asgName)
	}

	c.log.Info("DELETE ASG polling for completion", zap.String("asgName", asgName))
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		c.log.Error("DELETE ASG polling failed", zap.String("asgName", asgName), zap.Error(err))
		return errors.Wrapf(err, "deleting ASG %q", asgName)
	}

	c.log.Info("DELETE ASG succeeded", zap.String("asgName", asgName))
	return nil
}

// UpdateTags patches tags on an Application Security Group without modifying other properties.
func (c *ASGClient) UpdateTags(ctx context.Context, asgName string, tags map[string]*string) (*armnetwork.ApplicationSecurityGroup, error) {
	c.log.Info("PATCH ASG tags", zap.String("resourceGroup", c.resourceGroup), zap.String("asgName", asgName),
		zap.Int("tagCount", len(tags)))

	params := armnetwork.TagsObject{
		Tags: tags,
	}

	resp, err := c.client.UpdateTags(ctx, c.resourceGroup, asgName, params, nil)
	if err != nil {
		c.log.Error("PATCH ASG tags failed", zap.String("asgName", asgName), zap.Error(err))
		return nil, errors.Wrapf(err, "updating tags on ASG %q", asgName)
	}

	c.log.Info("PATCH ASG tags succeeded", zap.String("asgName", asgName),
		zap.String("id", ptrVal(resp.ID)), zap.String("provisioningState", provisioningStateVal(resp.Properties.ProvisioningState)))
	return &resp.ApplicationSecurityGroup, nil
}

// List returns all Application Security Groups in the resource group.
func (c *ASGClient) List(ctx context.Context) ([]*armnetwork.ApplicationSecurityGroup, error) {
	c.log.Info("LIST ASGs", zap.String("resourceGroup", c.resourceGroup))

	var asgs []*armnetwork.ApplicationSecurityGroup
	pager := c.client.NewListPager(c.resourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			c.log.Error("LIST ASGs failed", zap.Error(err))
			return nil, errors.Wrap(err, "listing ASGs")
		}
		asgs = append(asgs, page.Value...)
	}

	c.log.Info("LIST ASGs succeeded", zap.Int("count", len(asgs)))
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
			c.log.Error("LIST ALL ASGs failed", zap.Error(err))
			return nil, errors.Wrap(err, "listing all ASGs in subscription")
		}
		asgs = append(asgs, page.Value...)
	}

	c.log.Info("LIST ALL ASGs succeeded", zap.Int("count", len(asgs)))
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
