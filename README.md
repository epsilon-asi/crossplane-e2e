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

The harness writes its own kubeconfig (`$TMPDIR/xp-e2e-<cluster>.kubeconfig`);
it never changes your current context. `XP_E2E_CLUSTER` matches the cluster
name exactly; a reused cluster keeps the image and arguments it was created
with, and Crossplane and the providers are re-applied idempotently.

## Failures and cleanup

Each `ResourceTest` runs as the steps `create`, `managed resources become
[...]`, `external state after create`, `update: ...` and `delete and verify
external removal`, each its own line in `go test -v` output. When a step
fails, the remaining steps are skipped and the feature's Teardown deletes
whatever was created (managed resources first, then prerequisites). Panics in
checks are reported as failures rather than crashing the run.

A generated cluster is deleted when the suite ends, including when env setup
(Crossplane or provider install) fails, when the run is interrupted
(SIGINT/SIGTERM), and 30s before `go test -timeout` would kill the run. If
deletion fails the run fails and logs the `k3d cluster delete` command to run.

`providers/kubernetes/failures/` demonstrates this with tests that fail on
purpose. They are skipped unless `XP_E2E_FAILURE_DEMO` is set:

```bash
# Every TestExpectFail_* test FAILs; TestCleanupVerified must PASS and the
# run must end with "no leak: cluster ... no longer exists".
XP_E2E_FAILURE_DEMO=steps go test ./providers/kubernetes/failures/ -v -count=1 -timeout 20m
# env setup fails installing a provider that does not exist; the cluster must
# still be deleted.
XP_E2E_FAILURE_DEMO=env-setup go test ./providers/kubernetes/failures/ -v -count=1 -timeout 20m
```

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
