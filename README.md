# crossplane-e2e

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
| `XP_E2E_PACKAGE_<PROVIDER>` | Override a provider's package, e.g. `XP_E2E_PACKAGE_PROVIDER_KUBERNETES`. A registry reference or a package file (see below). |
| `XP_E2E_RUN_ID` | Use this run ID instead of a random one (lowercase letters, digits, dashes; max 20). Lets CI name a run before it starts and sweep it afterwards. |

The harness writes its own kubeconfig (`$TMPDIR/xp-e2e-<cluster>.kubeconfig`);
it never changes your current context. `XP_E2E_CLUSTER` matches the cluster
name exactly; a reused cluster keeps the image and arguments it was created
with, and Crossplane and the providers are re-applied idempotently.

### Testing a locally built package

A provider's `Package` can be the path of a package file, such as the
`.xpkg` a provider build produces, instead of a registry reference. The
harness then creates the cluster with its own registry
(`<cluster>-reg.localhost`, deleted with the cluster), pushes the file there
and installs the provider from it, so unpublished builds can be tested without
pushing them anywhere. Use the package built for the cluster's architecture
(`linux/arm64` on Apple Silicon, `linux/amd64` on most CI runners). Give an
absolute path: Go runs tests in their package's directory. A cluster reused
with `XP_E2E_CLUSTER` must have been created by a run that used a package
file.

```bash
XP_E2E_PACKAGE_PROVIDER_KUBERNETES="$PWD/_output/xpkg/linux_arm64/provider-kubernetes.xpkg" \
  go test ./providers/kubernetes/ -v -count=1 -timeout 20m
```

## Failures and cleanup

Each `ResourceTest` runs as the steps `create`, `managed resources become
[...]`, `external state after create`, `update: ...` and `delete and verify
external removal`, each its own line in `go test -v` output. When a step
fails, the remaining steps are skipped and the feature's Teardown deletes
whatever was created (managed resources first, then prerequisites). Panics in
checks are reported as failures rather than crashing the run.

A generated cluster is deleted when the suite ends, including when env setup
(Crossplane or provider install) fails, when the run is interrupted
(SIGINT/SIGTERM), and when `go test -timeout` is near. In every case the run's
managed resources are deleted first (see "Cleanup before cluster deletion"
below). If deletion fails the run fails and logs the `k3d cluster delete`
command to run.

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

## Using it from a provider's repo

Keep the provider's e2e tests in their own Go module (e.g. `e2e/go.mod`), so
the harness's Kubernetes dependencies don't constrain the provider's:

```bash
mkdir e2e && cd e2e
go mod init github.com/<org>/<provider>/e2e
go get github.com/epsilon-asi/crossplane-e2e@latest
```

Write the suite as described in "Adding a provider" below, with the published
package as `Package`. In CI, build the provider, then point the suite at the
package file the build produced:

```bash
export XP_E2E_PACKAGE_PROVIDER_FOO="$PWD/_output/xpkg/linux_amd64/provider-foo.xpkg"
export XP_E2E_RUN_ID="gh-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT"
cd e2e && go test ./... -v -count=1 -timeout 30m
```

Use an absolute path for the package: Go runs each test in its package's
directory.

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
