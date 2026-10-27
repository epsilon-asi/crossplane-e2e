package harness

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

// EnvPackagePrefix, followed by a provider's name in upper case with dashes
// as underscores, overrides that provider's Package, e.g.
// XP_E2E_PACKAGE_PROVIDER_KUBERNETES=_output/xpkg/linux_amd64/provider.xpkg.
const EnvPackagePrefix = "XP_E2E_PACKAGE_"

// PackageEnvVar returns the variable that overrides the named provider's
// package.
func PackageEnvVar(providerName string) string {
	return EnvPackagePrefix + strings.ToUpper(strings.ReplaceAll(providerName, "-", "_"))
}

// isLocalPackage reports whether pkg names a package file on disk (e.g. the
// .xpkg a provider's build produces) rather than a registry reference.
func isLocalPackage(pkg string) bool {
	st, err := os.Stat(pkg)
	return err == nil && st.Mode().IsRegular()
}

// looksLikePath reports whether pkg is meant as a file path rather than a
// registry reference, so a missing file is reported as such instead of being
// tried as a reference.
func looksLikePath(pkg string) bool {
	return strings.HasSuffix(pkg, ".xpkg") || strings.HasPrefix(pkg, "/") ||
		strings.HasPrefix(pkg, "./") || strings.HasPrefix(pkg, "../")
}

// Local packages are pushed to a registry that k3d creates with the cluster
// and deletes with it. Its name ends in .localhost because Crossplane can't
// be told a registry is insecure; its registry library uses plain HTTP for
// *.localhost names. k3d makes the name resolvable in the cluster and
// configures the nodes' containerd to pull from it over HTTP.
const registryPort = "5000"

func registryName(cluster string) string { return cluster + "-reg.localhost" }

// registryCreateArg is the k3d flag that creates the cluster's registry,
// published on a free loopback port for pushing from the host.
func registryCreateArg(cluster string) ([]string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("finding a free port for the registry: %w", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return []string{"--registry-create", fmt.Sprintf("%s:127.0.0.1:%d", registryName(cluster), port)}, nil
}

// registryHostAddr returns the host address the cluster's registry is
// published on, or an error if the cluster has no registry (e.g. a reused
// cluster created before a local package was used with it).
func registryHostAddr(ctx context.Context, cluster string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "port", registryName(cluster), registryPort+"/tcp").Output()
	if err != nil {
		return "", fmt.Errorf("cluster %q has no package registry %q (a reused cluster must have been created by a run that used a local package; delete it or use another name): %w",
			cluster, registryName(cluster), err)
	}
	addr := strings.TrimSpace(strings.Split(strings.TrimSpace(string(out)), "\n")[0])
	if addr == "" {
		return "", fmt.Errorf("registry %q publishes no port", registryName(cluster))
	}
	return addr, nil
}

// pushLocalPackage pushes the package file at path to the cluster's registry
// and returns the reference Crossplane should install it from.
func pushLocalPackage(ctx context.Context, cluster, repo, tag, path string) (string, error) {
	start := time.Now()
	img, err := tarball.ImageFromPath(path, nil)
	if err != nil {
		return "", fmt.Errorf("reading package %s: %w", path, err)
	}
	addr, err := registryHostAddr(ctx, cluster)
	if err != nil {
		return "", err
	}
	push, err := name.NewTag(fmt.Sprintf("%s/%s:%s", addr, repo, tag), name.Insecure)
	if err != nil {
		return "", err
	}
	if err := remote.Write(push, img, remote.WithContext(ctx)); err != nil {
		return "", fmt.Errorf("pushing package %s: %w", path, err)
	}
	ref := fmt.Sprintf("%s:%s/%s:%s", registryName(cluster), registryPort, repo, tag)
	log.Printf("pushed %s as %s in %s", path, ref, time.Since(start).Round(time.Second))
	return ref, nil
}
