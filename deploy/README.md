# Kueuecapacity Deployment

This directory contains Kubernetes manifests for deploying kueuecapacity.

## Files

- `rbac.yaml` - ServiceAccount, ClusterRole, and ClusterRoleBinding with minimal required permissions
- `deployment.yaml` - Minimal Deployment manifest with security best practices
- `kustomization.yaml` - Kustomize configuration for deploying all resources

## Required Permissions

The ClusterRole provides these minimal permissions based on the application's resource access patterns:

- **Nodes**: get, list, watch (monitors node capacity changes)
- **ClusterQueues**: get, update (retrieves and updates resource quotas)
- **ResourceFlavors**: get, list, watch (tracks resource flavor configurations)

## Deployment

```bash
# Deploy using kubectl
kubectl apply -k deploy/

# Or deploy individual manifests
kubectl apply -f deploy/rbac.yaml
kubectl apply -f deploy/deployment.yaml
```

## Configuration

The deployment uses these default settings:
- Monitors ClusterQueue: `default`
- Tracks resources: `cpu,memory,nvidia.com/gpu,rdma/ib`
- Runs in verbose mode
- Deployed to `kueue-system` namespace

Override using environment variables with `KC_` prefix or command line arguments.