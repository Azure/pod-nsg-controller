# Pod NSG Controller — Design Document

## 1. Problem Statement

Kubernetes clusters running on Azure use Network Security Groups (NSGs) to control inbound and outbound traffic at the virtual network level. However, NSG rules traditionally target subnets or network interfaces by IP address — they have no concept of pod identity. Since pods are ephemeral and their IP addresses change frequently, it is impractical to write static NSG rules that target individual pods. This applies equally to Azure Kubernetes Service (AKS) and self-managed Kubernetes clusters deployed on Azure VMs.

Pod NSG Controller solves this by introducing an indirection layer through **Azure Application Security Groups (ASGs)**. The controller dynamically allocates pod network interfaces to ASGs, and NSG rules reference those ASGs as source or destination. This enables **pod-level network security** — fine-grained isolation and security policies that follow pod identity rather than ephemeral IP addresses.

## 2. Goals

- Dynamically assign pod NICs to Azure Application Security Groups based on pod metadata (labels, annotations, namespace).
- Enable NSG rules that reference ASGs, providing pod-level network traffic control.
- Ensure idempotent, convergent reconciliation — the controller always drives toward the desired ASG membership state.
- Clean up ASG memberships when pods are deleted to prevent stale references.
- Minimize Azure API calls through intelligent caching and batching.
- Support high availability via leader election.

## 3. Non-Goals

- Replacing Kubernetes NetworkPolicy or Azure Network Policy Manager — this controller operates at the Azure infrastructure layer, complementing (not replacing) in-cluster network policies.
- Managing ASGs or NSGs for non-pod resources (VMs, load balancers, etc.).
- Authoring NSG rules automatically — the controller manages ASG membership; NSG rules referencing those ASGs are authored by the operator or via infrastructure-as-code.
- Multi-cluster federation or cross-subscription management (initial scope).

## 4. Architecture

### 4.1 High-Level Overview

```
┌──────────────────────────────────────────────────────────┐
│                  Kubernetes Cluster                       │
│                                                          │
│  ┌──────────────┐    watch     ┌───────────────────────┐ │
│  │  Kubernetes   │ ──────────► │  Pod NSG Controller   │ │
│  │  API Server   │   pod       │                       │ │
│  └──────────────┘   events     │  ┌─────────────────┐  │ │
│                                │  │ Pod Reconciler  │  │ │
│                                │  └────────┬────────┘  │ │
│                                └───────────┼───────────┘ │
│                                            │             │
└────────────────────────────────────────────┼─────────────┘
                                             │ Azure SDK
                                             ▼
                      ┌──────────────────────────────────────┐
                      │       Azure Resource Manager         │
                      │                                      │
                      │  ┌──────────────────┐                │
                      │  │ Application       │  pod NIC ──►  │
                      │  │ Security Groups   │  ASG member   │
                      │  └────────┬─────────┘                │
                      │           │ referenced by            │
                      │  ┌────────▼─────────┐                │
                      │  │ Network Security  │                │
                      │  │ Group (NSG)       │                │
                      │  │ Rules             │                │
                      │  └──────────────────┘                │
                      └──────────────────────────────────────┘
```

### 4.2 Core Concept: ASG-Based Pod Security

Azure Application Security Groups allow you to group network interfaces together and use those groups in NSG rules as source or destination. This decouples security policy from IP addresses.

**Example flow:**

1. An operator creates ASGs like `asg-frontend` and `asg-backend` in the resource group.
2. An operator creates NSG rules:
   - Allow `asg-frontend` → `asg-backend` on port 8080
   - Deny `asg-frontend` → `asg-backend` on all other ports
3. The controller watches pods and assigns their NICs:
   - Pods labeled `role=frontend` → `asg-frontend`
   - Pods labeled `role=backend` → `asg-backend`
4. As pods scale up/down, the controller updates ASG memberships. The NSG rules remain static — no rule changes needed.

### 4.3 Components

#### Controller Manager

The entry point that bootstraps the controller-runtime manager, registers controllers, configures leader election, and starts health/metrics servers.

#### Pod Reconciler

Watches Pod resources and triggers reconciliation when pods are created, updated, or deleted. The reconciler:

1. Reads pod labels and annotations to determine desired ASG membership.
2. Resolves the pod's underlying Azure network interface (NIC) using node and pod network metadata.
3. Fetches the NIC's current ASG associations from Azure.
4. Computes the diff between desired and actual ASG memberships.
5. Updates the NIC's ASG associations via the Azure SDK.
6. On pod deletion, removes the NIC from all controller-managed ASGs.

#### Azure ASG Client

A wrapper around the Azure SDK for Go (`armnetwork.InterfacesClient`, `armnetwork.ApplicationSecurityGroupsClient`) that provides:

- NIC-to-ASG association management (add/remove NIC from ASGs).
- Caching of current NIC and ASG state to reduce API calls.
- Retry logic with exponential backoff for transient Azure errors.
- Rate limiting to stay within Azure API throttling limits.

#### Configuration Loader

Reads configuration from environment variables, command-line flags, and optionally a ConfigMap. Validates required parameters at startup.

### 4.4 Annotation and Label Schema

Pods declare desired ASG membership via labels and annotations:

```yaml
metadata:
  labels:
    pod-nsg-controller.azure.com/asg: "frontend"
  annotations:
    pod-nsg-controller.azure.com/asgs: "asg-frontend,asg-shared-services"
```

- **Label `pod-nsg-controller.azure.com/asg`**: Assigns the pod to a single ASG (simple case).
- **Annotation `pod-nsg-controller.azure.com/asgs`**: Comma-separated list for multi-ASG membership.

Namespace-level annotations can define defaults that apply to all pods in the namespace unless overridden at the pod level.

### 4.5 ASG Naming and Discovery

The controller discovers ASGs by name within the configured resource group. ASGs must be pre-created by the operator (or via infrastructure-as-code). The controller does not create or delete ASGs — it only manages NIC membership.

ASG names referenced in pod annotations must match existing ASGs in the resource group. If an ASG does not exist, the controller logs a warning and skips that assignment.

## 5. Reconciliation Logic

```
On Pod Event (Create/Update/Delete):
  1. Resolve the pod's Azure NIC from node/pod network metadata
  2. If pod is being deleted:
     a. Remove NIC from all controller-managed ASGs
  3. Else:
     a. Read desired ASG memberships from pod labels/annotations
     b. Resolve ASG resource IDs from names
     c. Fetch current NIC ASG associations (from cache or Azure)
     d. Diff desired vs. actual:
        - ASGs in desired but not actual → ADD NIC to ASG
        - ASGs in actual but not desired → REMOVE NIC from ASG
     e. Update NIC configuration via Azure SDK
  4. Update cache
  5. If any Azure call fails → requeue with backoff
```

### 5.1 NIC Resolution

Pods run on nodes backed by Virtual Machine Scale Sets (VMSS) or Virtual Machines. In both AKS and self-managed clusters, the controller resolves a pod's NIC by:

1. Identifying the node the pod is scheduled on.
2. Querying Azure for the node's VMSS instance or VM.
3. Locating the NIC associated with the pod's IP address on that node.

For Azure CNI (pod-level NIC assignment), each pod has a dedicated NIC. For kubenet or overlay networking, the controller maps via the node's primary NIC.

### 5.2 Conflict Resolution

- **Concurrent NIC updates**: The controller uses Azure ETags for optimistic concurrency when updating NIC configurations.
- **Race conditions**: Leader election ensures only one instance performs writes at a time.
- **External ASG memberships**: The controller only manages ASG associations it owns (tracked via annotations on the NIC or an internal state map). Manually added ASG memberships are preserved.

## 6. Authentication and Authorization

The controller authenticates to Azure using one of the following methods:

- **Azure Workload Identity** (recommended for AKS) — provides a pod-level managed identity without storing credentials. See [Workload Identity documentation](https://learn.microsoft.com/en-us/azure/aks/workload-identity-overview).
- **Managed Identity** — for self-managed clusters running on Azure VMs with a system-assigned or user-assigned managed identity.
- **Service Principal** — for environments where managed identity is not available. Credentials are provided via environment variables or a mounted secret.

Required Azure RBAC role assignments:

| Role | Scope | Purpose |
|---|---|---|
| Network Contributor | Resource group | Manage NIC-to-ASG associations and read NSG state |
| Reader | Resource group | List ASGs, NICs, and VMSS instances |

## 7. Observability

### Metrics (Prometheus)

| Metric | Type | Description |
|---|---|---|
| `podnsg_reconcile_total` | Counter | Total reconciliation attempts |
| `podnsg_reconcile_errors_total` | Counter | Failed reconciliations |
| `podnsg_reconcile_duration_seconds` | Histogram | Reconciliation latency |
| `podnsg_azure_api_calls_total` | Counter | Azure API calls by operation |
| `podnsg_asg_memberships_total` | Gauge | Number of active pod-to-ASG memberships |
| `podnsg_nic_updates_total` | Counter | NIC configuration updates performed |

### Health Probes

- `/healthz` — Liveness probe (controller process is running)
- `/readyz` — Readiness probe (controller can reach Azure and the Kubernetes API)

### Logging

Structured JSON logging via `logr`/`zap`. Key log fields: `namespace`, `pod`, `node`, `nic`, `asg`, `operation`.

## 8. Error Handling and Resilience

| Failure Mode | Mitigation |
|---|---|
| Azure API throttling (429) | Exponential backoff with jitter; respect `Retry-After` header |
| Azure transient errors (5xx) | Retry up to 3 times with backoff |
| ASG not found | Log warning, skip assignment, do not requeue |
| NIC resolution failure | Log error, requeue with backoff (node may not be ready yet) |
| Stale cache | Periodic full resync every 5 minutes |
| Controller crash | Leader election ensures new leader picks up; all state is in Azure + Kubernetes |
| Invalid annotations | Log warning on the pod event, skip ASG assignment, do not requeue |

## 9. Security Considerations

- **Least privilege**: The controller only requires Network Contributor on the target resource group, not the entire subscription.
- **No secrets in annotations**: Annotations contain only ASG names — no credentials, tokens, or sensitive data.
- **Input validation**: All annotation values are validated (ASG name format, existence) before making Azure API calls.
- **Audit trail**: Azure Activity Log captures all NIC and ASG modifications made by the controller's identity.
- **RBAC**: The controller's Kubernetes ServiceAccount has minimal RBAC (get/list/watch pods and nodes).
- **ASGs are operator-managed**: The controller cannot create or delete ASGs, preventing privilege escalation through annotation injection.

## 10. Future Considerations

- **Namespace-level ASG defaults**: Allow namespace annotations to define default ASG memberships for all pods.
- **CRD-based configuration**: Introduce an `ApplicationSecurityGroupBinding` CRD for richer configuration beyond annotations.
- **Automatic ASG creation**: Optionally create ASGs on demand based on labels (with appropriate RBAC).
- **Multi-NSG support**: Manage ASG memberships across multiple NSGs based on node pool or subnet mapping.
- **Webhook validation**: Admission webhook to validate ASG annotations before pod creation.
- **Cross-subscription support**: Manage ASGs in different subscriptions for hub-spoke topologies.
