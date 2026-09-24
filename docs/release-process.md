# Pod NSG Controller Release Process

Pod NSG Controller releases use a build-once, validate-by-digest, promote-by-digest
model. A release tag starts the official 1ES pipeline; the image reaching MCR is
the same immutable candidate that passed the multi-cluster validation gate.

```text
approved vX.Y.Z tag
  -> unit and envtest validation
  -> linux/amd64 + linux/arm64 binaries
  -> OneBranch external-distribution signing and SBOM
  -> multi-architecture candidate image from signed binaries
  -> two-cluster release validation
  -> Container Networking lead approval
  -> digest-preserving promotion to MCR
```

## Release invariants

1. Release tags use `vMAJOR.MINOR.PATCH` and are created only through
   `.github/workflows/create-release-tag.yml`.
2. `scripts/release/build-binaries.sh` injects the release tag into the binary.
   The signed AMD64 binary must print the same tag with `--version`.
3. `Dockerfile.release` contains only the already-built, already-signed binary.
   It never recompiles source.
4. The candidate is referenced by digest throughout validation and promotion.
5. MCR promotion is available only for tag builds after all validation stages
   succeed and the `container-networking-lead-approval` environment is approved.
6. The target MCR digest must equal the validated candidate digest.

## Multi-cluster release gate

The release gate deploys the candidate to two externally managed self-hosted
Kubernetes clusters matching [multi-cluster-test-setup.md](multi-cluster-test-setup.md).
It then runs `make test-release-multicluster`, covering:

| Test | Required result |
|---|---|
| Single-cluster scale-up | Both mappings are `Synced`; prefix sets exactly match the running pod IPs |
| Concurrent writes | Both clusters own distinct prefix sets in the same ASGs; no controller `412` errors |
| Scale-down and cleanup | Remaining IPs are exact and the second cluster's prefix sets are deleted |
| Parallel scale-up | 25 + 10 pod IPs converge across four prefix sets without conflicts |

The test infrastructure is intentionally environment-owned. The pipeline deploys
the exact candidate controller on every run but does not recreate the underlying
VM-based Kubernetes clusters. Reusing the controlled environment avoids a release
result depending on several hours of infrastructure provisioning and allows the
same cluster identities and cross-subscription RBAC assignments to be reviewed.

## Required pipeline configuration

The Azure DevOps pipeline must be onboarded to the official 1ES template and
configured with:

- `BUILD_POOL_1ESPT_AMD`
- `VALIDATION_ACR_SERVICE_CONNECTION`
- `VALIDATION_ACR_LOGIN_SERVER`
- `VALIDATION_ACR_USERNAME` and secret `VALIDATION_ACR_PASSWORD`
- secure files named by `CLUSTER_A_KUBECONFIG_SECURE_FILE` and
  `CLUSTER_B_KUBECONFIG_SECURE_FILE`
- `CLUSTER_A_NAME`, `CLUSTER_A_SUBSCRIPTION_ID`, `CLUSTER_A_RESOURCE_GROUP`
- `CLUSTER_B_NAME`, `CLUSTER_B_SUBSCRIPTION_ID`, `CLUSTER_B_RESOURCE_GROUP`
- `RELEASE_CLUSTER_A_ASG_RESOURCE_IDS` and
  `RELEASE_CLUSTER_B_ASG_RESOURCE_IDS`, each containing the backend and frontend
  ASG resource IDs in the order documented by the E2E test contract
- MCR publisher credentials `MCR_USERNAME` and secret `MCR_PASSWORD`

Configure Azure DevOps environment
`container-networking-lead-approval` with the Container Networking release leads
as required approvers, disable requester self-approval, and require all checks to
pass. Configure GitHub environment `container-networking-tag-approval` with the
same ownership policy for tag creation.

## Starting a release

1. Choose a commit from `main` for which the normal CI pipeline is green.
2. Run **Create Release Tag** with a new semantic version, the full commit SHA,
   and the release reason.
3. Approve the protected tag environment.
4. Monitor `.pipelines/pipeline.yaml`. Do not approve MCR promotion until the
   multi-cluster stage has completed and its logs show all four test cases pass.
5. Approve `container-networking-lead-approval`.
6. Verify the published image:

   ```bash
   crane digest mcr.microsoft.com/containernetworking/pod-nsg-controller:vX.Y.Z
   docker run --rm \
     mcr.microsoft.com/containernetworking/pod-nsg-controller:vX.Y.Z \
     --version
   ```

The digest must match the candidate digest recorded by the pipeline, and the
binary must print the release tag.
