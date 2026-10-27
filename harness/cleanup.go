package harness

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/env"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// EnvKeepResources keeps the run's managed (and so external) resources at the
// end of the run, e.g. to debug them. It implies keeping the cluster, since
// deleting the cluster would orphan them. Remember to clean up afterwards.
const EnvKeepResources = "XP_E2E_KEEP_RESOURCES"

// DefaultCleanupTimeout bounds each of the end-of-run deletion of managed
// resources and the sweepers.
const DefaultCleanupTimeout = 5 * time.Minute

// safety is the Suite state behind credentials gating, end-of-run cleanup and
// interrupt handling.
type safety struct {
	// ctx is the root context of the run. It is cancelled on SIGINT/SIGTERM,
	// or shortly before go test's -timeout, to stop tests early while
	// leaving time for cleanup.
	ctx    context.Context
	cancel context.CancelFunc

	skipReason    string
	keepResources bool
	stopWatching  func()
	interrupted   atomic.Bool
	leaked        atomic.Bool
}

func (s *Suite) initSafety() {
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.env = s.env.WithContext(s.ctx)
	s.keepResources = envTrue(os.LookupEnv, EnvKeepResources)
}

func (s *Suite) cleanupTimeout() time.Duration {
	if s.cfg.CleanupTimeout > 0 {
		return s.cfg.CleanupTimeout
	}
	return DefaultCleanupTimeout
}

// requirements are the suite's own plus the variables its credentials need.
func (s *Suite) requirements() Requirements {
	req := s.cfg.Requires
	for _, p := range s.cfg.Providers {
		for _, c := range p.Credentials {
			req = req.merge(Requirements{Env: c.Env})
		}
	}
	return req
}

// preRun handles the cases where the suite does not run its environment: a
// standalone sweep, and unmet suite requirements. Otherwise it starts
// watching for interrupts and returns done=false.
func (s *Suite) preRun(m *testing.M) (code int, done bool) {
	if v := os.Getenv(EnvSweep); v != "" {
		return s.sweepOnly(v), true
	}
	switch v, reason := s.requirements().check(os.LookupEnv); v {
	case fail:
		log.Printf("suite cannot run: %s", reason)
		return 1, true
	case skip:
		// Run the tests anyway so that each one reports SKIP with the reason;
		// Suite.Test skips them before touching a cluster.
		s.skipReason = reason
		log.Printf("skipping suite, no cluster created: %s", reason)
		return m.Run(), true
	}
	s.watchInterrupts()
	return 0, false
}

// postRun stops watching for interrupts and fails the run if it was
// interrupted or may have leaked external resources.
func (s *Suite) postRun(code *int) {
	s.stopWatching()
	if s.leaked.Load() {
		log.Printf("FAIL: run %s may have leaked external resources (see above); sweep with %s=%s",
			s.vars["RUN_ID"], EnvSweep, s.vars["RUN_ID"])
	}
	if (s.leaked.Load() || s.interrupted.Load()) && *code == 0 {
		*code = 1
	}
}

func (s *Suite) skipIfUnavailable(t *testing.T) {
	t.Helper()
	if s.skipReason != "" {
		t.Skip("skipping (whole suite): " + s.skipReason)
	}
}

// watchInterrupts cancels the run's context on the first SIGINT/SIGTERM, or
// when go test's -timeout is close, so in-flight tests stop and the Finish
// steps (managed resource cleanup, sweepers, cluster deletion) still run. A
// second signal exits immediately.
func (s *Suite) watchInterrupts() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	s.stopWatching = func() {
		signal.Stop(sigs)
		close(done)
	}

	var deadline <-chan time.Time
	// Managed resources and sweepers each get cleanupTimeout, plus some time
	// to delete the cluster.
	budget := 2*s.cleanupTimeout() + 2*time.Minute
	if d := guardAfter(testTimeout(), budget); d > 0 {
		timer := time.NewTimer(d)
		deadline = timer.C
		go func() { <-done; timer.Stop() }()
	}

	go func() {
		select {
		case sig := <-sigs:
			log.Printf("received %s: stopping tests and cleaning up before exiting; send it again to exit immediately (may leak external resources)", sig)
		case <-deadline:
			log.Printf("go test -timeout is near: stopping tests now to leave time for cleanup")
		case <-done:
			return
		}
		s.interrupted.Store(true)
		s.cancel()
		select {
		case sig := <-sigs:
			id := s.vars["RUN_ID"]
			msg := fmt.Sprintf("received %s again: exiting without cleanup. Run %s may have leaked external resources; sweep them with %s=%s go test <package>. Managed resources: kubectl get managed -A -l %s=%s",
				sig, id, EnvSweep, id, LabelRunID, id)
			if !s.cfg.KeepCluster {
				msg += fmt.Sprintf(". Delete the cluster with: k3d cluster delete %s", s.cfg.ClusterName)
			}
			log.Print(msg)
			os.Exit(130)
		case <-done:
		}
	}()
}

// testTimeout returns go test's -timeout, or 0 if there is none.
func testTimeout() time.Duration {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return 0
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return 0
	}
	d, _ := g.Get().(time.Duration)
	return d
}

// guardAfter is how long after the start of the run to stop tests so that
// cleanup can finish before go test's timeout kills the process (which skips
// all cleanup). It reserves budget for cleanup but never stops tests before
// half the timeout. 0 means no guard.
func guardAfter(timeout, budget time.Duration) time.Duration {
	if timeout <= 0 {
		return 0
	}
	return max(timeout-budget, timeout/2)
}

// cleanupRun is a Finish step that runs before the cluster is deleted. It
// deletes managed resources, so that their providers delete the external
// resources, waits for them to be gone, and runs the suite's sweepers scoped
// to this run. When the cluster will be deleted every managed resource in it
// is deleted; when the cluster is kept, only those labelled with this run's
// ID.
//
// It returns a context that is no longer cancelled by an interrupt, so that
// the Finish steps after it (cluster deletion) run too.
func (s *Suite) cleanupRun() env.Func {
	return func(ctx context.Context, cfg *envconf.Config) (context.Context, error) {
		if ctx == nil {
			ctx = s.ctx
		}
		ctx = context.WithoutCancel(ctx)
		id := s.vars["RUN_ID"]
		if s.keepResources {
			log.Printf("%s=true: keeping managed resources of run %s in cluster %s. List them with: kubectl get managed -A -l %s=%s. Clean up by deleting them, or with %s=%s",
				EnvKeepResources, id, s.cfg.ClusterName, LabelRunID, id, EnvSweep, id)
			return ctx, nil
		}

		// Errors are logged here; e2e-framework would only log them at -v=2.
		if err := s.cleanupManagedResources(ctx, cfg); err != nil {
			s.leaked.Store(true)
			log.Printf("cleanup: %v", err)
		}
		// Sweepers get their own time budget: they matter most when deleting
		// managed resources timed out.
		sctx, cancel := context.WithTimeout(ctx, s.cleanupTimeout())
		defer cancel()
		if sweepers, err := selectSweepers(s.cfg.Sweepers, ""); err != nil {
			s.leaked.Store(true)
			log.Printf("cleanup: %v", err)
		} else if err := runSweepers(sctx, sweepers, SweepScope{RunID: id}, os.LookupEnv, false); err != nil {
			s.leaked.Store(true)
			log.Printf("cleanup: %v", err)
		}
		return ctx, nil
	}
}

func (s *Suite) cleanupManagedResources(ctx context.Context, cfg *envconf.Config) error {
	ctx, cancel := context.WithTimeout(ctx, s.cleanupTimeout())
	defer cancel()
	c, err := s.clusterClient(cfg)
	if err != nil {
		// Typically cluster creation failed, so nothing was created in it.
		log.Printf("cleanup: skipping managed resources: %v", err)
		return nil
	}
	id := s.vars["RUN_ID"]
	sel, what := labels.SelectorFromSet(labels.Set{LabelRunID: id}), "of run "+id
	if !s.cfg.KeepCluster {
		sel, what = labels.Everything(), "in cluster "+s.cfg.ClusterName
	}
	if err := deleteManagedResources(ctx, c, sel, what); err != nil {
		return err
	}
	if !s.cfg.KeepCluster {
		return nil
	}
	// Don't leave credentials at rest in a kept cluster, now that nothing
	// needs them to delete external resources any more.
	return deleteCredentialSecrets(ctx, c, id)
}

// clusterClient returns a client for the suite's cluster only. It refuses
// to fall back to any other kubeconfig or context (e.g. the user's default),
// since cleanup deletes things.
func (s *Suite) clusterClient(cfg *envconf.Config) (client.Client, error) {
	path := cfg.KubeconfigFile()
	if path == "" {
		return nil, errors.New("no kubeconfig for the cluster")
	}
	kctx := "k3d-" + s.cfg.ClusterName
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: path},
		&clientcmd.ConfigOverrides{CurrentContext: kctx},
	).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig %s, context %s: %w", path, kctx, err)
	}
	return client.New(rc, client.Options{})
}

// managedKinds returns the kinds of every installed managed resource CRD:
// those in the "managed" category, which Crossplane providers use for all
// managed resources (it is what `kubectl get managed` lists).
func managedKinds(ctx context.Context, c client.Client) ([]schema.GroupVersionKind, error) {
	crds := &unstructured.UnstructuredList{}
	crds.SetGroupVersionKind(schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinitionList"})
	if err := c.List(ctx, crds); err != nil {
		return nil, fmt.Errorf("listing CRDs: %w", err)
	}
	var out []schema.GroupVersionKind
	for _, crd := range crds.Items {
		cats, _, _ := unstructured.NestedStringSlice(crd.Object, "spec", "names", "categories")
		if !slices.Contains(cats, "managed") {
			continue
		}
		group, _, _ := unstructured.NestedString(crd.Object, "spec", "group")
		kind, _, _ := unstructured.NestedString(crd.Object, "spec", "names", "kind")
		versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
		if v := storageVersion(versions); v != "" {
			out = append(out, schema.GroupVersionKind{Group: group, Version: v, Kind: kind})
		}
	}
	return out, nil
}

func storageVersion(versions []any) string {
	for _, v := range versions {
		m, _ := v.(map[string]any)
		if storage, _ := m["storage"].(bool); storage {
			name, _ := m["name"].(string)
			return name
		}
	}
	return ""
}

func listManaged(ctx context.Context, c client.Client, kinds []schema.GroupVersionKind, sel labels.Selector) ([]unstructured.Unstructured, error) {
	var out []unstructured.Unstructured
	for _, gvk := range kinds {
		l := &unstructured.UnstructuredList{}
		l.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		if err := c.List(ctx, l, client.MatchingLabelsSelector{Selector: sel}); err != nil {
			if meta.IsNoMatchError(err) {
				continue
			}
			return nil, fmt.Errorf("listing %s: %w", gvk.Kind, err)
		}
		out = append(out, l.Items...)
	}
	return out, nil
}

// deleteManagedResources deletes the managed resources matching sel and waits
// until they are gone, i.e. their providers have deleted the external
// resources and removed their finalizers.
func deleteManagedResources(ctx context.Context, c client.Client, sel labels.Selector, what string) error {
	start := time.Now()
	kinds, err := managedKinds(ctx, c)
	if err != nil {
		return err
	}
	mrs, err := listManaged(ctx, c, kinds, sel)
	if err != nil {
		return err
	}
	if len(mrs) == 0 {
		log.Printf("cleanup: no managed resources left %s", what)
		return nil
	}
	log.Printf("cleanup: deleting %d managed resources %s", len(mrs), what)
	var errs []error
	for i := range mrs {
		mr := &mrs[i]
		if orphans(mr) {
			log.Printf("cleanup: WARNING %s does not delete its external resource (deletionPolicy/managementPolicies); external-name=%q will be left behind",
				describe(mr), mr.GetAnnotations()["crossplane.io/external-name"])
		}
		log.Printf("cleanup: deleting %s", describe(mr))
		if err := client.IgnoreNotFound(c.Delete(ctx, mr)); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", describe(mr), err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}

	var left []unstructured.Unstructured
	err = wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		l, err := listManaged(ctx, c, kinds, sel)
		if err != nil {
			return false, nil // transient; keep trying until the deadline
		}
		left = l
		return len(l) == 0, nil
	})
	if err != nil {
		var b strings.Builder
		for i := range left {
			mr := &left[i]
			fmt.Fprintf(&b, "\n  %s external-name=%q\n%s", describe(mr), mr.GetAnnotations()["crossplane.io/external-name"], summarizeConditions(mr))
		}
		return fmt.Errorf("%d managed resources %s were not deleted (%v); their external resources may leak:%s", len(left), what, err, b.String())
	}
	log.Printf("cleanup: %d managed resources %s deleted in %s", len(mrs), what, time.Since(start).Round(time.Second))
	return nil
}

// orphans reports whether deleting mr leaves its external resource behind.
func orphans(mr *unstructured.Unstructured) bool {
	if p, _, _ := unstructured.NestedString(mr.Object, "spec", "deletionPolicy"); p == "Orphan" {
		return true
	}
	policies, found, _ := unstructured.NestedStringSlice(mr.Object, "spec", "managementPolicies")
	if !found || len(policies) == 0 {
		return false
	}
	return !slices.Contains(policies, "*") && !slices.Contains(policies, "Delete")
}

// deleteCredentialSecrets deletes the Secrets created from environment
// variables by this run.
func deleteCredentialSecrets(ctx context.Context, c client.Client, runID string) error {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "SecretList"})
	if err := c.List(ctx, l, client.MatchingLabels{LabelCredentials: "true", LabelRunID: runID}); err != nil {
		return fmt.Errorf("listing credential Secrets: %w", err)
	}
	var errs []error
	for i := range l.Items {
		if err := client.IgnoreNotFound(c.Delete(ctx, &l.Items[i])); err != nil {
			errs = append(errs, fmt.Errorf("deleting %s: %w", describe(&l.Items[i]), err))
		}
	}
	if len(l.Items) > 0 {
		log.Printf("cleanup: deleted %d credential Secrets of run %s", len(l.Items), runID)
	}
	return errors.Join(errs...)
}
