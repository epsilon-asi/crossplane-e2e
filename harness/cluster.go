package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
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

// The harness drives the k3d CLI on PATH directly rather than through
// e2e-framework's k3d support: that package decides whether a cluster exists
// with a prefix match on `k3d cluster get` output, so asking for cluster "xp"
// while "xp-test" exists would try to reuse (and later delete) the wrong one.

func k3d(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "k3d", args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("k3d %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ClusterExists reports whether a k3d cluster with exactly this name exists.
func ClusterExists(ctx context.Context, name string) (bool, error) {
	out, err := k3d(ctx, "cluster", "list", "-o", "json")
	if err != nil {
		return false, err
	}
	return listHasCluster([]byte(out), name)
}

// listHasCluster reports whether `k3d cluster list -o json` output lists a
// cluster with exactly this name.
func listHasCluster(list []byte, name string) (bool, error) {
	var clusters []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(list, &clusters); err != nil {
		return false, fmt.Errorf("parsing k3d cluster list: %w", err)
	}
	for _, c := range clusters {
		if c.Name == name {
			return true, nil
		}
	}
	return false, nil
}

// kubeconfigPath is where the harness keeps the cluster's kubeconfig. It is
// stable per cluster name so reuse runs overwrite rather than accumulate files.
func kubeconfigPath(name string) string {
	return filepath.Join(os.TempDir(), "xp-e2e-"+name+".kubeconfig")
}

// createCluster creates the named k3d cluster, or reuses it if a cluster with
// exactly that name exists, and points the environment's client at it. With
// registry, a new cluster gets a registry for local packages.
func createCluster(name, image string, registry bool) env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		start := time.Now()
		exists, err := ClusterExists(ctx, name)
		if err != nil {
			return ctx, err
		}
		if exists {
			// Starting a running cluster is a no-op; this covers a stopped one.
			// The existing cluster's image and arguments are kept as they are.
			log.Printf("reusing existing cluster %q", name)
			if _, err := k3d(ctx, "cluster", "start", name); err != nil {
				return ctx, fmt.Errorf("starting k3d cluster %q: %w", name, err)
			}
		} else {
			args := append([]string{"cluster", "create", name, "--image", image}, k3dArgs...)
			if registry {
				reg, err := registryCreateArg(name)
				if err != nil {
					return ctx, err
				}
				args = append(args, reg...)
			}
			if _, err := k3d(ctx, args...); err != nil {
				return ctx, fmt.Errorf("creating k3d cluster %q: %w", name, err)
			}
		}
		kc, err := k3d(ctx, "kubeconfig", "get", name)
		if err != nil {
			return ctx, err
		}
		path := kubeconfigPath(name)
		if err := os.WriteFile(path, []byte(kc), 0o600); err != nil {
			return ctx, fmt.Errorf("writing kubeconfig: %w", err)
		}
		cfg.WithKubeconfigFile(path)
		log.Printf("cluster %q ready in %s (kubeconfig %s)", name, time.Since(start).Round(time.Second), path)
		return ctx, nil
	}
}

// destroyCluster deletes the named cluster if it exists and confirms it is
// gone. It is safe to call more than once.
func destroyCluster(ctx context.Context, name string) error {
	exists, err := ClusterExists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		if _, err := k3d(ctx, "cluster", "delete", name); err != nil {
			return err
		}
		if exists, err := ClusterExists(ctx, name); err != nil || exists {
			return errors.Join(err, fmt.Errorf("cluster %q still listed after k3d cluster delete", name))
		}
		log.Printf("cluster %q deleted", name)
	} else {
		log.Printf("cluster %q does not exist; nothing to delete", name)
	}
	if err := os.Remove(kubeconfigPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("removing kubeconfig: %v", err)
	}
	return nil
}
