# crossplane-test-environment

End-to-end tests for Crossplane providers on ephemeral k3s clusters (via k3d).

Each provider suite gets a fresh cluster with Crossplane and the provider
installed, exercises managed resources through create → update → delete, and
checks the external system at each step. The cluster is deleted afterwards.

## Requirements

Docker, `k3d` (v5.9+), `helm`, Go.

## Running

```bash
go test ./providers/kubernetes/ -v -count=1 -timeout 20m
```

| Variable | Effect |
|---|---|
| `XP_E2E_CLUSTER=<name>` | Reuse (or create) this cluster and keep it afterwards. |
| `XP_E2E_KEEP=true` | Keep the generated cluster after the run. |
| `XP_E2E_K3S_IMAGE` | Override the k3s node image. |
| `XP_E2E_CROSSPLANE_VERSION` | Override the Crossplane chart version. |

The harness writes its own kubeconfig; it never changes your current context.

## Layout

- `harness/` — provider-agnostic: cluster, Crossplane and provider install,
  manifest apply/delete, and the managed resource lifecycle (`ResourceTest`).
- `providers/<name>/` — one Go test package per provider: a `TestMain` that
  declares the provider to install, test manifests under `testdata/`, and the
  checks against that provider's external system.

## Adding a provider

1. Create `providers/<name>/` with a `TestMain` calling `harness.NewSuite`
   with the provider package, any pre-install manifests (runtime config,
   RBAC) and post-install manifests (ProviderConfigs).
2. For each resource, add a `harness.ResourceTest` with its manifest and
   `Created` / `Updates` / `Deleted` checks against the external system.
3. Use `${RUN_ID}` in manifests to give resources names unique to the run.
