# kueuecapacity

Automatically sync Kubernetes node capacity with Kueue ClusterQueue quotas.

## Problem

Kueue ClusterQueues use static `nominalQuota` values that don't reflect actual
cluster capacity. In environments with fixed hardware that experiences failures,
this leads to inaccurate scheduling decisions.

## Solution

`kueuecapacity` monitors node capacity in real-time and updates ClusterQueue
quotas to match. It groups nodes by ResourceFlavor labels and aggregates their
resources.

## Installation

```bash
kubectl apply -k deploy/
```

Or use the Docker image directly:
```bash
docker pull ghcr.io/abatilo/kueuecapacity:latest
```

## Usage

```bash
# Monitor default ClusterQueue with default resources
kueuecapacity

# Monitor specific ClusterQueue and resources
kueuecapacity --cluster-queue=gpu-queue --resources=cpu,memory,nvidia.com/gpu

# Filter nodes by label
kueuecapacity --label-selector="node-role.kubernetes.io/worker=true"

# Dry-run mode (display only, no updates)
kueuecapacity --dry-run

# Verbose logging
kueuecapacity --verbose
```

## Configuration

| Flag | Environment Variable | Default | Description |
|------|---------------------|---------|-------------|
| `--cluster-queue` | `KC_CLUSTER_QUEUE` | `default` | ClusterQueue to update |
| `--resources` | `KC_RESOURCES` | `cpu,memory,nvidia.com/gpu,rdma/ib` | Resources to track |
| `--label-selector` | `KC_LABEL_SELECTOR` | `""` | Node label filter |
| `--dry-run` | `KC_DRY_RUN` | `false` | Display capacity without updating |
| `--verbose` | `KC_VERBOSE` | `false` | Enable debug logging |
| `--kubeconfig` | `KC_KUBECONFIG` | `~/.kube/config` | Kubeconfig path |

## How It Works

1. **Watches nodes** - Uses Kubernetes informers to monitor node add/update/delete events
2. **Groups by flavor** - Matches nodes to ResourceFlavors based on node labels
3. **Calculates capacity** - Aggregates resources for each ResourceFlavor group
4. **Updates quotas** - Sets ClusterQueue `nominalQuota` to match calculated capacity
5. **Skips unchanged** - Only updates when quotas actually change

## Requirements

- Kubernetes cluster with Kueue installed
- ClusterQueue with ResourceGroups matching tracked resources
- ResourceFlavors with nodeLabels for node grouping
- RBAC permissions (see `deploy/rbac.yaml`)

## Development

```bash
# Run locally
go run ./cmd/kueuecapacity --dry-run

# Run tests
go test ./...

# Build binary
go build -o kueuecapacity ./cmd/kueuecapacity
```
