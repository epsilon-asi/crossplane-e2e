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

## Paid / credentialed providers

Suites for providers that call real (possibly billable) APIs declare what
they need; a plain `go test ./...` never needs credentials and never spends
money.

| Variable | Effect |
|---|---|
| *(suite-defined, e.g. `ACME_TOKEN`)* | Credentials. When missing, the tests (or whole suite) that need them are skipped. |
| `XP_E2E_PAID=true` | Opt in to tests marked `Paid`. Without it they always skip. |
| `XP_E2E_STRICT=true` | Missing credentials fail instead of skip (use in CI jobs that should have them). |
| `XP_E2E_KEEP_RESOURCES=true` | Keep the run's managed/external resources (and so the cluster) for debugging. |
| `XP_E2E_SWEEP=all` or `=<run id>` | Run only the suite's sweepers: no cluster, no tests. |
| `XP_E2E_SWEEP_RUN=a,b` | Limit a sweep to these sweepers (plus their dependencies). |

```go
suite = harness.NewSuite(harness.Config{
	Providers: []harness.Provider{{
		Name: "provider-acme", Package: "xpkg.example.com/provider-acme:v1.0.0",
		// Secret crossplane-system/acme-creds, created before PostInstall.
		Credentials: []harness.SecretFromEnv{{
			Name: "acme-creds", Env: []string{"ACME_TOKEN"},
			Template: `{"token": {{ json .ACME_TOKEN }}}`,
		}},
		PostInstall: []string{"testdata/setup/providerconfig.yaml"},
	}},
	Sweepers: []harness.Sweeper{{
		Name: "buckets", Requires: []string{"ACME_TOKEN"},
		Sweep: func(ctx context.Context, scope harness.SweepScope) error {
			// list buckets via the API; delete those where scope.Matches(name)
		},
	}},
})

func TestBucket(t *testing.T) {
	harness.Require(t, harness.Requirements{Paid: true}) // skip unless XP_E2E_PAID=true
	suite.Test(t, harness.ResourceTest{...})
}
```

Safety model:

- **Gating.** Credential variables of `Provider.Credentials` and
  `Config.Requires` gate the whole suite: if any is unset the suite is skipped
  before a cluster is created. `harness.Require` gates single tests. Paid
  tests skip unless `XP_E2E_PAID=true`, even with `XP_E2E_STRICT`.
- **Secrets.** Values go only into the cluster Secret. The harness never logs
  them or writes them to disk, and redacts them from its errors. Don't log
  them in your own checks. A kept cluster holds them until the run's cleanup
  deletes them.
- **Labels.** Everything the harness applies is labelled
  `xp-e2e/run-id=<RUN_ID>`: `kubectl get managed -A -l xp-e2e/run-id=<id>`.
- **Cleanup before cluster deletion.** A Finish step deletes managed
  resources and waits (up to `Config.CleanupTimeout`, default 5m) until their
  providers have deleted the external resources, *then* the cluster is
  deleted. For an ephemeral cluster that is every managed resource in it; for
  a kept cluster (`XP_E2E_CLUSTER`, `XP_E2E_KEEP`) only this run's, since
  keeping the cluster is about the infrastructure, not the billable resources.
  Resources that can't be deleted are listed with their external names and
  fail the run.
- **Interrupts.** Ctrl-C/SIGTERM stops the tests and still runs cleanup and
  cluster deletion; a second Ctrl-C exits immediately and prints what may have
  leaked. Tests are likewise stopped shortly before `go test -timeout` (which
  would otherwise kill the process with no cleanup), so give `-timeout`
  headroom.
- **Sweepers** delete external resources by name directly through the
  provider's API, as a backstop for runs that died (SIGKILL, CI cancellation,
  lost cluster). Name external resources `e2e-${RUN_ID}-...` so that
  `SweepScope.Matches` finds them. Sweepers run scoped to the run at the end of
  every run, and on their own with
  `XP_E2E_SWEEP=all go test ./providers/<name>/ -v -count=1` (every `e2e-`
  resource, including those of runs still in progress elsewhere: don't sweep
  `all` while other runs share the account) or `XP_E2E_SWEEP=<run id>`. In CI,
  run a sweep step after the tests even when they fail or are cancelled.
