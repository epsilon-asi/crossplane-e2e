package harness

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// Provider is a Crossplane provider package to install for a suite.
type Provider struct {
	// Name of the Provider object.
	Name string
	// Package is the fully qualified package reference, e.g.
	// xpkg.crossplane.io/crossplane-contrib/provider-kubernetes:v1.4.1, or
	// the path of a package file such as the .xpkg a provider build produces,
	// which is pushed to a registry inside the cluster. PackageEnvVar(Name)
	// overrides it at run time.
	Package string
	// RuntimeConfigName references a DeploymentRuntimeConfig, typically one
	// created by PreInstall.
	RuntimeConfigName string
	// PreInstall manifests are applied before the Provider, e.g. a
	// DeploymentRuntimeConfig and RBAC for the provider's service account.
	PreInstall []string
	// Credentials are Secrets created from environment variables after
	// PreInstall and before the provider and PostInstall. Their variables
	// are requirements of the whole suite.
	Credentials []SecretFromEnv
	// PostInstall manifests are applied once the provider is healthy, e.g.
	// ProviderConfigs.
	PostInstall []string
	// Timeout for the provider to become healthy. Defaults to 5 minutes.
	Timeout time.Duration
}

func (p Provider) object() *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "pkg.crossplane.io/v1",
		"kind":       "Provider",
		"metadata":   map[string]any{"name": p.Name},
		"spec":       map[string]any{"package": p.Package},
	}}
	if p.RuntimeConfigName != "" {
		_ = unstructured.SetNestedMap(u.Object, map[string]any{
			"apiVersion": "pkg.crossplane.io/v1beta1",
			"kind":       "DeploymentRuntimeConfig",
			"name":       p.RuntimeConfigName,
		}, "spec", "runtimeConfigRef")
	}
	return u
}

// installProvider installs the provider package, waits until it is installed
// and healthy, then applies its PostInstall manifests.
func installProvider(p Provider, vars map[string]string, cluster string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		start := time.Now()
		if p.Timeout == 0 {
			p.Timeout = 5 * time.Minute
		}
		r, err := cfg.NewClient()
		if err != nil {
			return ctx, err
		}
		c := r.Resources().GetControllerRuntimeClient()

		pre, err := LoadAll(p.PreInstall, vars)
		if err != nil {
			return ctx, err
		}
		post, err := LoadAll(p.PostInstall, vars)
		if err != nil {
			return ctx, err
		}

		if err := Apply(ctx, c, pre); err != nil {
			return ctx, fmt.Errorf("provider %s: %w", p.Name, err)
		}
		if err := applyCredentials(ctx, c, p.Credentials, os.LookupEnv, runLabels(vars)); err != nil {
			return ctx, fmt.Errorf("provider %s credentials: %w", p.Name, err)
		}
		if isLocalPackage(p.Package) {
			ref, err := pushLocalPackage(ctx, cluster, p.Name, "e2e-"+vars["RUN_ID"], p.Package)
			if err != nil {
				return ctx, fmt.Errorf("provider %s: %w", p.Name, err)
			}
			p.Package = ref
		}
		pkg := p.object()
		addLabels([]*unstructured.Unstructured{pkg}, runLabels(vars))
		if err := Apply(ctx, c, []*unstructured.Unstructured{pkg}); err != nil {
			return ctx, fmt.Errorf("provider %s: %w", p.Name, err)
		}
		if err := WaitForConditions(ctx, c, []*unstructured.Unstructured{pkg}, []string{"Installed", "Healthy"}, p.Timeout); err != nil {
			return ctx, fmt.Errorf("provider %s: %w", p.Name, err)
		}
		if err := Apply(ctx, c, post); err != nil {
			return ctx, fmt.Errorf("provider %s: %w", p.Name, err)
		}
		log.Printf("provider %s (%s) healthy in %s", p.Name, p.Package, time.Since(start).Round(time.Second))
		return ctx, nil
	}
}
