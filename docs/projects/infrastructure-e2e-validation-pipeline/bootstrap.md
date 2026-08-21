# E2E validation & release — one-time bootstrap (cross-subscription scope)

> Scope note: this file currently documents the **cross-subscription (`xs`)
> identity, RBAC, and pre-provisioning preflight** introduced by **EPIC-010
> (ITEM-036)**. The full one-time setup guide — public/staging registries,
> release-environment controls, and the transparent-tunnel CNI pin policy — is
> authored by **EPIC-008 (ITEM-026)** and will extend this document. Everything
> below is a durable, out-of-band operator action; the pipeline never creates
> these resources per run (CON-007/CON-008).

The pipeline authenticates to Azure with **GitHub OIDC only** — no long-lived
cloud secret is ever stored (NFR-005/SEC-001). The `xs` topology spans **two
distinct subscriptions**, so it uses **two separate federated identities**, one
per subscription, and passes an explicit `--subscription` on every Azure
operation (RD-020). This bounds blast radius and eliminates mutable
`az account` context mistakes.

## 1. Separate OIDC federated identities (one per subscription)

Create one least-privilege app registration / user-assigned identity **per
subscription** and federate each to this repository's `azure-e2e` GitHub
Environment (SEC-002/RD-020).

| Role | Subscription | GitHub secrets | GitHub variable |
|------|--------------|----------------|-----------------|
| Primary (Cluster A + shared ASGs) | `primary_subscription_id` | `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` | `E2E_PRIMARY_SUBSCRIPTION_ID` |
| Secondary (Cluster B, `xs` only) | `secondary_subscription_id` | `AZURE_SECONDARY_CLIENT_ID`, `AZURE_SECONDARY_TENANT_ID` | `E2E_SECONDARY_SUBSCRIPTION_ID` |

- `E2E_SECONDARY_SUBSCRIPTION_ID` **MUST** be a separately configured Environment
  variable and **MUST differ** from the primary; no secondary default is hard-coded
  into workflow source (CON-002). A run may override either via the
  `primary_subscription_id` / `secondary_subscription_id` workflow inputs.
- Federated-credential subject: scope each credential to this repo and the
  `azure-e2e` environment (and `public-release` for the release identity).

### Least privilege (subscription-level bootstrap role)

Each per-subscription federated identity gets a **custom role** limited to the
run lifecycle — never `Owner` (SEC-002):

- create/delete **tagged** run resource groups (`Microsoft.Resources/subscriptions/resourceGroups/*`);
- assign/remove the approved runtime role (`Microsoft.Authorization/roleAssignments/write|delete`)
  **scoped to run resource groups** so the workflow can grant/revoke the runtime
  VM grants created by `scripts/e2e/setup-cross-sub-rbac.sh`.

Runtime cluster-node (VM managed) identities receive only **`Network Contributor`
on the primary run resource group** (RG-scoped, never subscription-scoped) so the
controller can write the shared ASGs; Cluster B's grant crosses the subscription
boundary. These grants are created, inventoried by assignment ID, and later
removed/verified by the pipeline (FR-026/XSUB-003/AC-027).

## 2. Cross-cutting job authentication (both subscriptions)

Jobs that touch a **single** subscription log in once with that subscription's
identity. Jobs that must touch **both** subscriptions in one job —
`rbac_xs` (read Cluster B principals in secondary, grant on primary), the
`validate_cross_subscription` seam, `diagnostics_xs`, and `cleanup_xs` —
authenticate to **each** identity with two `azure/login@v2` steps. Because every
script passes an explicit `--subscription`, the correct cached OIDC token is
selected per call; no script relies on the active-account context (RD-020).

## 3. Pre-provisioning preflight (fail before provisioning)

Before **any** `xs` resource is created, the `preflight_xs` job fails closed unless
all of the following pass for the relevant subscription(s) (FR-025/AC-024). The
checks are implemented as side-effect-free helpers in `scripts/e2e/lib.sh`:

| Check | Helper | Fails when |
|-------|--------|-----------|
| Distinct subscriptions | `lib::assert_distinct_subscriptions <primary> <secondary>` | IDs empty or equal |
| Tenant/subscription validation | `lib::az_subscription_validate <az> <sub> [tenant]` | subscription inaccessible / not `Enabled` / tenant mismatch |
| Network provider + EUAP API | `lib::az_provider_preflight <az> <sub> Microsoft.Network applicationSecurityGroups 2025-07-01` | `Microsoft.Network` not `Registered`, or the `addressPrefixSets` api-version `2025-07-01` is unavailable (CON-001) |
| EUAP VM quota | `lib::az_vm_quota_preflight <az> <sub> <region> standardDSv5Family 16` | insufficient family/total vCPUs for 1 control-plane + 3 workers |

`meta` (pure computation, no cloud) additionally fails the run early if a release
omits either topology or if the `xs` subscription IDs are equal (CON-009/FR-025);
`preflight_xs` adds the cloud-touching identity, provider, and quota gates for
**both** subscriptions.

## 4. Verify (dry run)

```bash
# distinct + accessible + provider/API + quota, per subscription (uses real `az`)
source scripts/e2e/lib.sh
lib::assert_distinct_subscriptions "$PRIMARY_SUBSCRIPTION_ID" "$SECONDARY_SUBSCRIPTION_ID"
lib::az_subscription_validate az "$PRIMARY_SUBSCRIPTION_ID"
lib::az_subscription_validate az "$SECONDARY_SUBSCRIPTION_ID"
lib::az_provider_preflight   az "$PRIMARY_SUBSCRIPTION_ID"   Microsoft.Network applicationSecurityGroups 2025-07-01
lib::az_provider_preflight   az "$SECONDARY_SUBSCRIPTION_ID" Microsoft.Network
lib::az_vm_quota_preflight   az "$PRIMARY_SUBSCRIPTION_ID"   eastus2euap    standardDSv5Family 16
lib::az_vm_quota_preflight   az "$SECONDARY_SUBSCRIPTION_ID" centraluseuap  standardDSv5Family 16
```

All commands pass an explicit `--subscription` and never run `az account set`.
