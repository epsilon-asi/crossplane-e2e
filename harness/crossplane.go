package harness

import (
	"context"
	"fmt"
	"log"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/third_party/helm"
)

// CrossplaneOptions configures the Crossplane Helm release.
type CrossplaneOptions struct {
	// Version of the crossplane chart. Defaults to DefaultCrossplaneVersion.
	Version string
	// Repo is the Helm chart repository URL.
	Repo string
	// Namespace Crossplane is installed into.
	Namespace string
	// HelmArgs are passed through to `helm upgrade --install`, e.g.
	// "--set", "provider.defaultActivations={}".
	HelmArgs []string
}

// Crossplane defaults.
const (
	DefaultCrossplaneVersion = "2.4.3"
	DefaultCrossplaneRepo    = "https://charts.crossplane.io/stable"
	DefaultCrossplaneNS      = "crossplane-system"
)

func (o *CrossplaneOptions) setDefaults() {
	if o.Version == "" {
		o.Version = DefaultCrossplaneVersion
	}
	if o.Repo == "" {
		o.Repo = DefaultCrossplaneRepo
	}
	if o.Namespace == "" {
		o.Namespace = DefaultCrossplaneNS
	}
}

// installCrossplane installs (or upgrades) Crossplane and waits for it to be
// available. The chart is fetched by URL so the user's Helm repos are untouched.
func installCrossplane(o CrossplaneOptions) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		start := time.Now()
		args := append([]string{"--install", "--create-namespace", "--repo", o.Repo}, o.HelmArgs...)
		err := helm.New(cfg.KubeconfigFile()).RunUpgrade(
			helm.WithContext(ctx),
			helm.WithName("crossplane"),
			helm.WithChart("crossplane"),
			helm.WithNamespace(o.Namespace),
			helm.WithVersion(o.Version),
			helm.WithArgs(args...),
			helm.WithWait(),
			helm.WithTimeout("5m"),
		)
		if err != nil {
			return ctx, fmt.Errorf("installing crossplane %s: %w", o.Version, err)
		}
		log.Printf("crossplane %s ready in %s", o.Version, time.Since(start).Round(time.Second))
		return ctx, nil
	}
}
