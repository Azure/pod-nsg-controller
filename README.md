# Pod NSG Controller

Pod NSG Controller is a Kubernetes controller that enables **pod-level network security** for Kubernetes clusters running on Azure by managing **Azure Network Security Groups (NSGs)** through **Dynamic Application Security Groups (ASGs)**. It works with both self-managed Kubernetes clusters and Azure Kubernetes Service (AKS).

## Overview

In Kubernetes clusters running on Azure, network traffic is governed by NSGs attached to the underlying virtual network, but NSG rules traditionally operate at the subnet or NIC level — not at the pod level. Pod NSG Controller bridges this gap by dynamically allocating pods to Azure Application Security Groups (ASGs) and using those ASGs when defining NSG rules. This enables fine-grained network isolation and security policies scoped to individual pods or groups of pods.

### How It Works

1. The controller watches pod lifecycle events in the Kubernetes cluster.
2. When a pod is created or updated, the controller determines desired ASG membership from pod labels and annotations.
3. NSG rules reference these ASGs as source or destination, enabling pod-level traffic control without managing individual IP addresses.
4. When a pod is deleted, the controller removes its NIC from the ASG, ensuring stale memberships are cleaned up.

### Key Features

- **Dynamic ASG allocation** — Automatically assigns pod NICs to Application Security Groups based on pod metadata.
- **Pod-level NSG rules** — NSG rules reference ASGs instead of IP addresses, enabling security policies that follow pod identity rather than ephemeral IPs.
- **Annotation and label-driven configuration** — Define ASG membership through pod labels and annotations.
- **Idempotent operations** — Safe to restart or run multiple replicas; the controller converges to the desired state without duplicating ASG memberships.
- **Azure SDK integration** — Uses the official Azure SDK for Go to interact with the Azure Resource Manager API.
- **Leader election** — Supports leader election for high-availability deployments.

## Prerequisites

- Go 1.23+
- A Kubernetes cluster (1.27+) running on Azure (AKS or self-managed)
- Azure identity with permissions to manage ASGs and NSG rules on the target resource group
- `kubectl` configured to access the target cluster
- [kubebuilder](https://book.kubebuilder.io/) (for development)

## Getting Started

### Build

```bash
make build
```

### Run Locally (against a cluster)

```bash
# Ensure your kubeconfig points to the target cluster
export AZURE_SUBSCRIPTION_ID=<subscription-id>
export AZURE_RESOURCE_GROUP=<resource-group>
export AZURE_NSG_NAME=<nsg-name>

make run
```

### Deploy to Cluster

```bash
# Build and push the container image
make docker-build docker-push IMG=<registry>/pod-nsg-controller:<tag>

# Deploy the controller
make deploy IMG=<registry>/pod-nsg-controller:<tag>
```

### Uninstall

```bash
make undeploy
```

## Configuration

The controller is configured via a combination of environment variables and command-line flags.

The following environment variables are supported:

| Variable | Description | Required |
|---|---|---|
| `AZURE_SUBSCRIPTION_ID` | Azure subscription containing the NSG and ASGs | Yes |
| `AZURE_RESOURCE_GROUP` | Resource group containing the NSG and ASGs | Yes |
| `AZURE_NSG_NAME` | Name of the NSG to manage rules on | Yes |

Command-line flags are used to configure other settings such as leader election and the metrics/health probe bind addresses. Refer to the `cmd/` package and deployment manifests for the exact flags and defaults.
## Project Structure

```
├── cmd/                  # Application entrypoint
├── internal/
│   ├── controller/       # Kubernetes controller reconciliation logic
│   ├── azure/            # Azure SDK client wrappers for ASG and NSG operations
│   └── config/           # Configuration loading and validation
├── config/               # Kubernetes manifests (RBAC, deployment, etc.)
├── docs/                 # Design and architecture documents
├── Dockerfile
├── Makefile
└── go.mod
```

## Development

```bash
# Run tests
make test

# Run linter
make lint

# Generate manifests and code
make generate manifests
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines on how to contribute to this project.

## Support

See [SUPPORT.md](SUPPORT.md) for information on how to get help.

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.

## Data Collection

The software may collect information about you and your use of the software and
send it to Microsoft. Microsoft may use this information to provide services and
improve our products and services. You may turn off the telemetry as described
in the repository. There are also some features in the software that may enable
you and Microsoft to collect data from users of your applications. If you use
these features, you must comply with applicable law, including providing
appropriate notices to users of your applications together with a copy of
Microsoft's privacy statement. Our privacy statement is located at
<https://go.microsoft.com/fwlink/?LinkID=824704>. You can learn more about data
collection and use in the help documentation and our privacy statement. Your use
of the software operates as your consent to these practices.

## Trademarks

This project may contain trademarks or logos for projects, products, or services. Authorized use of Microsoft trademarks or logos is subject to and must follow [Microsoft's Trademark & Brand Guidelines](https://www.microsoft.com/en-us/legal/intellectualproperty/trademarks/usage/general). Use of Microsoft trademarks or logos in modified versions of this project must not cause confusion or imply Microsoft sponsorship. Any use of third-party trademarks or logos is subject to those third-party's policies.
