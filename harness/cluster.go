package harness

import (
	"context"
	"fmt"
	"log"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/support/k3d"
)

// DefaultK3sImage is the k3s node image used when none is configured.
const DefaultK3sImage = "rancher/k3s:v1.37.1-k3s1"

// k3dArgs keep the cluster lean and, importantly, keep k3d away from the
// user's default kubeconfig: the harness uses a kubeconfig file of its own.
var k3dArgs = []string{
	"--no-lb",
	"--k3s-arg", "--disable=traefik@server:0",
	"--k3s-arg", "--disable=servicelb@server:0",
	"--kubeconfig-update-default=false",
	"--kubeconfig-switch-context=false",
	"--wait", "--timeout", "180s",
}

func newK3dCluster(name string) *k3d.Cluster {
	c := k3d.NewCluster(name)
	// Without an explicit path e2e-framework `go install`s its own pinned
	// k3d instead of using the one on PATH.
	c.WithPath("k3d")
	return c
}

// createCluster creates the named k3d cluster, or reuses it if it exists, and
// points the environment's client at it.
func createCluster(name, image string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		start := time.Now()
		args := append([]string{"--image", image}, k3dArgs...)
		kubeconfig, err := newK3dCluster(name).Create(ctx, args...)
		if err != nil {
			return ctx, fmt.Errorf("creating k3d cluster %q: %w", name, err)
		}
		cfg.WithKubeconfigFile(kubeconfig)
		log.Printf("cluster %q ready in %s (kubeconfig %s)", name, time.Since(start).Round(time.Second), kubeconfig)
		return ctx, nil
	}
}

func deleteCluster(name string) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		if err := newK3dCluster(name).Destroy(ctx); err != nil {
			return ctx, fmt.Errorf("deleting k3d cluster %q: %w", name, err)
		}
		log.Printf("cluster %q deleted", name)
		return ctx, nil
	}
}
