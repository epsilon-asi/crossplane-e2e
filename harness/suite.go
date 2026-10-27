// Package harness runs Crossplane provider end-to-end tests against an
// ephemeral k3d (k3s) cluster.
//
// A provider suite declares what to install (Config) in TestMain and then
// describes managed resources to exercise (ResourceTest). The harness owns
// the cluster, Crossplane and the provider; suites only own their manifests
// and the provider-specific checks against the external system.
package harness

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"strconv"
	"testing"

	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// Environment variables that override Config at run time.
const (
	// EnvCluster reuses (or creates) a cluster with this name and keeps it
	// afterwards. Useful for a fast edit/run loop.
	EnvCluster = "XP_E2E_CLUSTER"
	// EnvKeep keeps a generated cluster after the run, e.g. to debug a failure.
	EnvKeep = "XP_E2E_KEEP"
	// EnvK3sImage overrides Config.K3sImage.
	EnvK3sImage = "XP_E2E_K3S_IMAGE"
	// EnvCrossplaneVersion overrides Config.Crossplane.Version.
	EnvCrossplaneVersion = "XP_E2E_CROSSPLANE_VERSION"
)

// Config describes the environment a provider suite runs against.
type Config struct {
	// ClusterName of the k3d cluster. When empty a unique name is generated
	// and the cluster is deleted when the suite finishes.
	ClusterName string
	// KeepCluster skips cluster deletion.
	KeepCluster bool
	// K3sImage pins the k3s node image.
	K3sImage string

	Crossplane CrossplaneOptions
	Providers  []Provider

	// Vars are substituted for ${NAME} references in every manifest the
	// harness applies. RUN_ID is always set to a value unique to the run.
	Vars map[string]string
}

// Suite is a configured environment shared by the tests of one package.
type Suite struct {
	cfg  Config
	env  env.Environment
	vars map[string]string
	// cleanup deletes the generated cluster; nil when the cluster is kept.
	cleanup *clusterCleanup
}

// NewSuite builds a suite from cfg, applying environment variable overrides.
func NewSuite(cfg Config) *Suite {
	runID := randomID()

	if v := os.Getenv(EnvCluster); v != "" {
		cfg.ClusterName = v
		cfg.KeepCluster = true
	}
	if cfg.ClusterName == "" {
		cfg.ClusterName = "xp-e2e-" + runID
	}
	if v, err := strconv.ParseBool(os.Getenv(EnvKeep)); err == nil && v {
		cfg.KeepCluster = true
	}
	if v := os.Getenv(EnvK3sImage); v != "" {
		cfg.K3sImage = v
	}
	if v := os.Getenv(EnvCrossplaneVersion); v != "" {
		cfg.Crossplane.Version = v
	}
	if cfg.K3sImage == "" {
		cfg.K3sImage = DefaultK3sImage
	}
	cfg.Crossplane.setDefaults()

	vars := map[string]string{"RUN_ID": runID}
	for k, v := range cfg.Vars {
		vars[k] = v
	}

	envCfg, err := envconf.NewFromFlags()
	if err != nil {
		log.Fatalf("parsing e2e-framework flags: %v", err)
	}

	s := &Suite{cfg: cfg, env: env.NewWithConfig(envCfg), vars: vars}

	setup := []env.Func{createCluster(cfg.ClusterName, cfg.K3sImage), installCrossplane(cfg.Crossplane)}
	for _, p := range cfg.Providers {
		setup = append(setup, installProvider(p, vars))
	}
	s.env.Setup(setup...)
	if !cfg.KeepCluster {
		s.cleanup = &clusterCleanup{name: cfg.ClusterName}
		s.env.Finish(s.cleanup.finish)
	}
	return s
}

// Run runs the package's tests. Call it from TestMain.
func (s *Suite) Run(m *testing.M) int {
	log.Printf("cluster=%s keep=%t k3s=%s crossplane=%s run_id=%s",
		s.cfg.ClusterName, s.cfg.KeepCluster, s.cfg.K3sImage, s.cfg.Crossplane.Version, s.vars["RUN_ID"])
	if s.cleanup != nil {
		defer s.cleanup.watch()()
	}
	code := s.env.Run(m)
	if s.cleanup != nil && s.cleanup.err != nil && code == 0 {
		code = 1 // the cluster leaked
	}
	if s.cfg.KeepCluster {
		log.Printf("cluster %q kept; delete it with: k3d cluster delete %s", s.cfg.ClusterName, s.cfg.ClusterName)
	}
	return code
}

// Test runs a managed resource lifecycle test.
func (s *Suite) Test(t *testing.T, rt ResourceTest) {
	t.Helper()
	s.env.Test(t, rt.feature(s.vars))
}

// Vars returns the substitution variables, including RUN_ID.
func (s *Suite) Vars() map[string]string { return s.vars }

// ClusterName returns the name of the suite's k3d cluster.
func (s *Suite) ClusterName() string { return s.cfg.ClusterName }

// KeepCluster reports whether the cluster is kept after the run.
func (s *Suite) KeepCluster() bool { return s.cfg.KeepCluster }

// EnvConf returns the e2e-framework configuration (including the cluster's
// kubeconfig once env Setup has run), for tests that need their own client.
func (s *Suite) EnvConf() *envconf.Config { return s.env.EnvConf() }

func randomID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("reading random bytes: %v", err))
	}
	return hex.EncodeToString(b)
}
