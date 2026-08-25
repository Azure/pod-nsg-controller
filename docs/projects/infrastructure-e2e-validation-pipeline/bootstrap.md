# E2E validation and release bootstrap

This guide covers the durable, one-time prerequisites for
`.github/workflows/e2e-validation-release.yml` and
`.github/workflows/e2e-reaper.yml`. The pipeline never creates these
subscription identities, role definitions, registries, or GitHub Environment
controls per run (CON-007/CON-008).

The pipeline authenticates to Azure with GitHub OIDC only. Do not create client
secrets, registry passwords, or committed SAS URLs (NFR-005/SEC-001/SEC-006).
The cross-subscription (`xs`) topology uses two distinct subscriptions and two
distinct federated identities. Every Azure operation in the pipeline passes an
explicit subscription ID; bootstrap and troubleshooting commands should do the
same (RD-020).

## 1. Record the bootstrap inputs

Set local shell variables for the operator performing the one-time setup:

```bash
export GITHUB_OWNER="<owner>"
export GITHUB_REPOSITORY="pod-nsg-controller"
export PRIMARY_SUBSCRIPTION_ID="<primary-subscription-id>"
export SECONDARY_SUBSCRIPTION_ID="<secondary-subscription-id>"
export PRIMARY_TENANT_ID="<primary-tenant-id>"
export SECONDARY_TENANT_ID="<secondary-tenant-id>"
export PRIMARY_CLIENT_ID="<primary-federated-client-id>"
export SECONDARY_CLIENT_ID="<secondary-federated-client-id>"
export PRIMARY_PRINCIPAL_ID="<primary-service-principal-object-id>"
export SECONDARY_PRINCIPAL_ID="<secondary-service-principal-object-id>"
export STAGING_ACR="<private-staging-acr-login-server>"
export PUBLIC_ACR="<public-release-acr-login-server>"

test "${PRIMARY_SUBSCRIPTION_ID}" != "${SECONDARY_SUBSCRIPTION_ID}"
```

The supported design is two subscriptions in the same tenant. A cross-tenant
deployment requires an explicitly reviewed federation and RBAC design; do not
reuse the same client ID as an implicit fallback.

## 2. Create separate GitHub OIDC identities

Use one app registration or user-assigned managed identity per subscription.
The identity represented by `PRIMARY_CLIENT_ID` owns primary-subscription
provisioning, staging publication, and reviewed public release. The identity
represented by `SECONDARY_CLIENT_ID` can operate only in the secondary
subscription.

Create federated credentials with these exact GitHub subject forms:

| Identity | GitHub Environment | Federated subject |
|---|---|---|
| Primary | `azure-e2e` | `repo:<owner>/pod-nsg-controller:environment:azure-e2e` |
| Primary | `public-release` | `repo:<owner>/pod-nsg-controller:environment:public-release` |
| Secondary | `azure-e2e` | `repo:<owner>/pod-nsg-controller:environment:azure-e2e` |

Each credential uses issuer
`https://token.actions.githubusercontent.com` and audience
`api://AzureADTokenExchange`. Do not add branch-wide or pull-request subjects:
cloud jobs are intentionally protected by GitHub Environments and pull requests
must not receive an Azure token.

Example federated-credential document for an app registration:

```json
{
  "name": "github-azure-e2e",
  "issuer": "https://token.actions.githubusercontent.com",
  "subject": "repo:<owner>/pod-nsg-controller:environment:azure-e2e",
  "description": "Pod NSG controller E2E environment",
  "audiences": ["api://AzureADTokenExchange"]
}
```

Create it with `az ad app federated-credential create --id <app-object-id>
--parameters <credential-file>`. Create the second primary credential with the
`public-release` subject. For a user-assigned managed identity, create the same
issuer/subject/audience tuple with `az identity federated-credential create`.

## 3. Assign least-privilege Azure roles

Never assign `Owner`. Create purpose-specific custom roles and assign them only
at the scopes shown below. The role-definition `AssignableScopes` permits the
role to exist at a subscription; the role assignment is what grants access.

Azure RBAC does not make resource-group create/delete permissions conditional
on tags. The subscription-scoped permission therefore cannot itself enforce the
`validation-purpose=pnc-e2e` tag. The workflow enforces and audits the required
tag set, cleanup operates only on deterministic run names/recorded IDs, and the
reaper filters on the required tags. Review those controls whenever the role is
changed.

### 3.1 Infrastructure operator roles

Create one role definition in each subscription. Replace the assignable-scope
placeholder and give each definition a unique role name. The primary role needs
role-assignment write access because it grants VM managed identities `Network
Contributor` on the primary run resource group. The secondary role does not.

Primary role actions:

```json
{
  "Name": "Pod NSG E2E Primary Infrastructure Operator",
  "IsCustom": true,
  "Description": "Provision and reap tagged Pod NSG E2E resources and manage run-RG Network Contributor assignments.",
  "Actions": [
    "Microsoft.Resources/subscriptions/read",
    "Microsoft.Resources/subscriptions/resourceGroups/read",
    "Microsoft.Resources/subscriptions/resourceGroups/write",
    "Microsoft.Resources/subscriptions/resourceGroups/delete",
    "Microsoft.Resources/subscriptions/resourceGroups/resources/read",
    "Microsoft.Resources/providers/read",
    "Microsoft.Compute/locations/usages/read",
    "Microsoft.Compute/virtualMachines/*",
    "Microsoft.Compute/disks/*",
    "Microsoft.Network/networkSecurityGroups/*",
    "Microsoft.Network/virtualNetworks/*",
    "Microsoft.Network/publicIPAddresses/*",
    "Microsoft.Network/natGateways/*",
    "Microsoft.Network/networkInterfaces/*",
    "Microsoft.Network/applicationSecurityGroups/*",
    "Microsoft.Authorization/roleDefinitions/read",
    "Microsoft.Authorization/roleAssignments/read",
    "Microsoft.Authorization/roleAssignments/write",
    "Microsoft.Authorization/roleAssignments/delete"
  ],
  "NotActions": [],
  "DataActions": [],
  "NotDataActions": [],
  "AssignableScopes": ["/subscriptions/<primary-subscription-id>"]
}
```

Secondary role actions are identical except that
`Microsoft.Authorization/roleAssignments/write` is omitted:

```json
{
  "Name": "Pod NSG E2E Secondary Infrastructure Operator",
  "IsCustom": true,
  "Description": "Provision and reap tagged Pod NSG E2E resources in the secondary subscription.",
  "Actions": [
    "Microsoft.Resources/subscriptions/read",
    "Microsoft.Resources/subscriptions/resourceGroups/read",
    "Microsoft.Resources/subscriptions/resourceGroups/write",
    "Microsoft.Resources/subscriptions/resourceGroups/delete",
    "Microsoft.Resources/subscriptions/resourceGroups/resources/read",
    "Microsoft.Resources/providers/read",
    "Microsoft.Compute/locations/usages/read",
    "Microsoft.Compute/virtualMachines/*",
    "Microsoft.Compute/disks/*",
    "Microsoft.Network/networkSecurityGroups/*",
    "Microsoft.Network/virtualNetworks/*",
    "Microsoft.Network/publicIPAddresses/*",
    "Microsoft.Network/natGateways/*",
    "Microsoft.Network/networkInterfaces/*",
    "Microsoft.Network/applicationSecurityGroups/*",
    "Microsoft.Authorization/roleDefinitions/read",
    "Microsoft.Authorization/roleAssignments/read",
    "Microsoft.Authorization/roleAssignments/delete"
  ],
  "NotActions": [],
  "DataActions": [],
  "NotDataActions": [],
  "AssignableScopes": ["/subscriptions/<secondary-subscription-id>"]
}
```

Create and assign the definitions:

```bash
az role definition create --role-definition primary-role.json
az role definition create --role-definition secondary-role.json

az role assignment create \
  --assignee-object-id "${PRIMARY_PRINCIPAL_ID}" \
  --assignee-principal-type ServicePrincipal \
  --role "Pod NSG E2E Primary Infrastructure Operator" \
  --scope "/subscriptions/${PRIMARY_SUBSCRIPTION_ID}"

az role assignment create \
  --assignee-object-id "${SECONDARY_PRINCIPAL_ID}" \
  --assignee-principal-type ServicePrincipal \
  --role "Pod NSG E2E Secondary Infrastructure Operator" \
  --scope "/subscriptions/${SECONDARY_SUBSCRIPTION_ID}"
```

At runtime, the primary identity assigns the built-in `Network Contributor`
role to each run VM managed identity at exactly:

```text
/subscriptions/<primary-subscription-id>/resourceGroups/<primary-run-rg>
```

It must never grant a VM identity at subscription scope. Assignment IDs are
recorded in `run-manifest.json`, removed by cleanup, and checked by the reaper.

### 3.2 Registry roles

Keep registry permissions separate from infrastructure permissions:

- Assign the primary identity `AcrPush` on the staging ACR. Staging remains
  private and anonymous pull stays disabled.
- Grant the primary identity only the staging-registry control-plane actions
  needed to create/delete repository-scoped scope maps and tokens and generate
  their short-lived credentials. Scope this custom assignment to the staging
  ACR, not the subscription.
- Assign the primary identity `AcrPush` on the public ACR and a custom
  public-release role scoped to that registry with
  `Microsoft.ContainerRegistry/registries/read`,
  `Microsoft.ContainerRegistry/registries/write`, and
  `Microsoft.ContainerRegistry/registries/importImage/action`. These actions
  support digest import, immutable release tags, and anonymous-pull
  configuration.
- The secondary identity receives no staging or public registry role.

The staging token-manager role should contain only:

```text
Microsoft.ContainerRegistry/registries/read
Microsoft.ContainerRegistry/registries/tokens/read
Microsoft.ContainerRegistry/registries/tokens/write
Microsoft.ContainerRegistry/registries/tokens/delete
Microsoft.ContainerRegistry/registries/tokens/generateCredentials/action
Microsoft.ContainerRegistry/registries/scopeMaps/read
Microsoft.ContainerRegistry/registries/scopeMaps/write
Microsoft.ContainerRegistry/registries/scopeMaps/delete
```

If the registries use ACR ABAC repository permissions, use the equivalent
repository-scoped Reader/Writer roles instead of classic `AcrPush`; do not grant
registry `Contributor` as a shortcut.

## 4. Configure registries

Provision two durable registries before enabling the workflow:

| Registry | Visibility | Repository paths | Required behavior |
|---|---|---|---|
| Staging | Private | `candidate/pod-nsg-controller`, `candidate/pod-nsg-cni-transparent-tunnel` | Anonymous pull disabled; candidate tags are temporary; validation consumes digests |
| Public release | Anonymous pull | `pod-nsg-release-index`, `pod-nsg-controller`, `pod-nsg-cni-transparent-tunnel` | The signed index is the atomic completion marker and records both exact digests/version; direct artifacts share the immutable version |

The release workflow promotes the exact validated controller and CNI digests;
it never rebuilds either artifact. It prepares and verifies both direct
semantic tags, handles requested moving tags, then publishes
`pod-nsg-release-index:<semver>` last. Consumers and audits MUST treat that
signed, attested index tag—not visibility of either direct tag—as release
completion. Public access is enabled only on the public registry. Confirm that
organizational policy permits anonymous ACR pull before enabling releases.

## 5. Configure GitHub Environments

Create both Environments in the GitHub repository settings.

### `azure-e2e`

Configure these Environment secrets:

| Secret | Value |
|---|---|
| `AZURE_CLIENT_ID` | Primary federated identity client ID |
| `AZURE_TENANT_ID` | Primary tenant ID |
| `AZURE_SUBSCRIPTION_ID` | Primary subscription ID; retained for the build jobs' `azure/login` contract |
| `AZURE_SECONDARY_CLIENT_ID` | Secondary federated identity client ID |
| `AZURE_SECONDARY_TENANT_ID` | Secondary tenant ID |

Configure these Environment variables:

| Variable | Required value |
|---|---|
| `E2E_PRIMARY_SUBSCRIPTION_ID` | Primary subscription ID; must match `AZURE_SUBSCRIPTION_ID` |
| `E2E_SECONDARY_SUBSCRIPTION_ID` | Distinct secondary subscription ID |
| `E2E_STAGING_ACR` | Staging ACR login server, without scheme |
| `E2E_CNI_SOURCE_MODE` | `source` for production; `prebuilt` only under the checksum policy below |
| `E2E_CNI_SOURCE_REPO` | `https://github.com/Azure/azure-container-networking` or an approved mirror |
| `E2E_CNI_SOURCE_REF` | Reviewed immutable commit SHA (preferred) or immutable signed release tag |
| `E2E_CNI_BINARY_SHA256` | Expected lowercase SHA-256 of the built or prebuilt `azure-vnet` |
| `E2E_CNI_BINARY_URL` | Empty for `source`; pinned credential-free URL for `prebuilt` |
| `E2E_CNI_CONFLIST_URL` | Empty for `source`; pinned credential-free URL for `prebuilt` |

Require reviewers for `azure-e2e` if cloud cost or EUAP capacity policy requires
manual approval. Restrict deployment branches to the default branch and
approved release tags. Do not expose this Environment to pull requests from
forks.

### `public-release`

Configure the same primary `AZURE_CLIENT_ID` and `AZURE_TENANT_ID` secrets, and
these variables:

| Variable | Required value |
|---|---|
| `E2E_PRIMARY_SUBSCRIPTION_ID` | Primary subscription ID |
| `E2E_STAGING_ACR` | Private staging ACR login server |
| `E2E_PUBLIC_ACR` | Public ACR login server, without scheme |

Set at least one required reviewer who is authorized to publish. Prevent
self-review when policy requires separation of duties, restrict deployment to
protected release tags/default branch, and do not add bypass rules for the
workflow identity. Approval is the final human boundary after all validation
and verified cleanup gates pass.

Manual releases provide `moving_tags` per dispatch (for example
`latest,v1,v1.2`); `meta` validates each tag against the requested semantic
version before any cloud work. `run_full_validation=true` forces both
topologies without releasing. `keep_resources_on_failure=true` is honored only
for failed non-release provisioning/validation and is rejected for releases.

The final `artifact_cleanup` job runs after both topology cleanups and every
release/non-release terminal path. It deletes and verifies both per-run
candidate tags and all deterministic per-run ACR pull tokens using the explicit
staging ACR subscription. Its failure fails the workflow without erasing a
successfully uploaded release manifest. The scheduled reaper also sweeps old
run-correlated candidate tags and tokens.

## 6. Validate both subscriptions before the first run

The workflow performs these checks before provisioning. Run the same
side-effect-free checks during bootstrap:

```bash
source scripts/e2e/lib.sh

lib::assert_distinct_subscriptions \
  "${PRIMARY_SUBSCRIPTION_ID}" "${SECONDARY_SUBSCRIPTION_ID}"

lib::az_subscription_validate \
  az "${PRIMARY_SUBSCRIPTION_ID}" "${PRIMARY_TENANT_ID}"
lib::az_subscription_validate \
  az "${SECONDARY_SUBSCRIPTION_ID}" "${SECONDARY_TENANT_ID}"

# Both subscriptions need Network and Compute. The primary also hosts the
# addressPrefixSets API used by the controller and validation.
lib::az_provider_preflight \
  az "${PRIMARY_SUBSCRIPTION_ID}" Microsoft.Network applicationSecurityGroups 2025-07-01
lib::az_provider_preflight \
  az "${PRIMARY_SUBSCRIPTION_ID}" Microsoft.Compute
lib::az_provider_preflight \
  az "${SECONDARY_SUBSCRIPTION_ID}" Microsoft.Network
lib::az_provider_preflight \
  az "${SECONDARY_SUBSCRIPTION_ID}" Microsoft.Compute

# One control-plane plus three Standard_D4s_v5 workers requires 16 vCPUs.
lib::az_vm_quota_preflight \
  az "${PRIMARY_SUBSCRIPTION_ID}" eastus2euap standardDSv5Family 16
lib::az_vm_quota_preflight \
  az "${PRIMARY_SUBSCRIPTION_ID}" centraluseuap standardDSv5Family 16
lib::az_vm_quota_preflight \
  az "${SECONDARY_SUBSCRIPTION_ID}" centraluseuap standardDSv5Family 16
```

Also verify that:

1. `Microsoft.Network` and `Microsoft.Compute` are already registered in both
   subscriptions. The pipeline checks registration but does not register
   providers.
2. The primary subscription exposes the
   `Microsoft.Network/applicationSecurityGroups/addressPrefixSets` API version
   `2025-07-01`.
3. Both subscriptions can allocate `Standard_D4s_v5` in their assigned EUAP
   regions and have both family and total regional vCPU quota.
4. Azure Policy allows the required VMs, managed identities, NSGs, public IPs,
   NAT gateways, NIC secondary IP configurations, and required correlation
   tags.

## 7. CNI source pin and checksum update policy

Production uses `source` mode. The repository default currently identifies the
upstream source in `scripts/e2e/build-cni.sh`; the GitHub Environment values are
the reviewed production pin. Prefer a full immutable commit SHA over a moving
branch or mutable tag.

To update the pin:

1. Review the upstream commit/tag and transparent-tunnel conflist changes.
   Confirm `cni/azure-linux-transparent-tunnel.conflist` still declares
   `"mode": "transparent-tunnel"`.
2. Build `azure-vnet` for `linux/amd64` from that exact ref in a clean,
   controlled environment. Record `sha256sum azure-vnet`.
3. Update `E2E_CNI_SOURCE_REF` and `E2E_CNI_BINARY_SHA256` together. If the
   repository fallback pin changes, update the defaults in
   `scripts/e2e/build-cni.sh` in the same reviewed change.
4. Run the hermetic CNI packaging test and a no-push artifact build. Verify the
   generated artifact contains only `azure-vnet` and
   `azure-linux-transparent-tunnel.conflist`, the checksum matches, and the
   conflist mode assertion passes.
5. Require normal code-owner review. Never accept an update that changes only
   the checksum after a mismatch without independently validating the source
   bytes.

`prebuilt` mode is a controlled fallback only. Both URLs must be immutable,
HTTPS, and credential-free, and `E2E_CNI_BINARY_SHA256` is mandatory. Never put
a SAS token or other credential in a GitHub variable, workflow, script, or
documentation.

## 8. Bootstrap completion checklist

- Primary and secondary IDs are non-empty and distinct.
- Each identity has only its own subscription role; only the primary identity
  can create run-RG role assignments.
- Runtime VM grants are `Network Contributor` on the primary run RG only.
- Staging is private; public ACR anonymous pull is organizationally approved.
- The public release index repository is writable/signable and anonymously
  readable alongside both direct artifact repositories.
- `azure-e2e` and `public-release` have the documented reviewers, branch/tag
  restrictions, secrets, and variables.
- Provider/API/quota checks pass in both subscriptions without `az account set`.
- The CNI ref and checksum were independently reviewed and recorded together.
- No client secret, registry password, kubeconfig, token, or credential-bearing
  URL is stored in GitHub configuration.
