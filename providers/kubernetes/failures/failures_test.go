// Package failures checks that the harness cleans up after failures. Its
// tests fail ON PURPOSE, so they only run when XP_E2E_FAILURE_DEMO is set:
//
//	XP_E2E_FAILURE_DEMO=steps     go test ./providers/kubernetes/failures/ -v -count=1 -timeout 20m
//	XP_E2E_FAILURE_DEMO=env-setup go test ./providers/kubernetes/failures/ -v -count=1 -timeout 20m
//
// "steps" runs features that fail in different steps (every TestExpectFail_*
// test should FAIL), then TestCleanupVerified (should PASS) checks that none
// of them left anything behind in the cluster. "env-setup" adds a provider
// package that does not exist, so env Setup fails. In both modes TestMain
// finally checks that the generated cluster was deleted, and exits with code
// 3 if it leaked.
package failures

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/epsilon-asi/crossplane-e2e/harness"
)

// EnvDemo selects the failure demo mode: "steps" or "env-setup".
const EnvDemo = "XP_E2E_FAILURE_DEMO"

var suite *harness.Suite

func TestMain(m *testing.M) {
	mode := os.Getenv(EnvDemo)
	if mode == "" {
		log.Printf("skipping deliberately failing tests: set %s=steps or %s=env-setup to run them", EnvDemo, EnvDemo)
		os.Exit(0)
	}
	cfg := harness.Config{
		Providers: []harness.Provider{{
			Name:              "provider-kubernetes",
			Package:           "xpkg.crossplane.io/crossplane-contrib/provider-kubernetes:v1.4.1",
			RuntimeConfigName: "provider-kubernetes",
			PreInstall:        []string{"../testdata/setup/runtime.yaml"},
			PostInstall:       []string{"../testdata/setup/providerconfig.yaml"},
		}},
	}
	switch mode {
	case "steps":
	case "env-setup":
		cfg.Providers = append(cfg.Providers, harness.Provider{
			Name:    "provider-does-not-exist",
			Package: "xpkg.crossplane.io/crossplane-contrib/provider-does-not-exist:v0.0.1",
			Timeout: 30 * time.Second,
		})
	default:
		log.Fatalf("%s=%q: want steps or env-setup", EnvDemo, mode)
	}
	suite = harness.NewSuite(cfg)
	code := suite.Run(m)

	if !suite.KeepCluster() {
		exists, err := harness.ClusterExists(context.Background(), suite.ClusterName())
		switch {
		case err != nil:
			log.Printf("checking for leaked cluster: %v", err)
			code = 3
		case exists:
			log.Printf("LEAK: cluster %q still exists after the run", suite.ClusterName())
			code = 3
		default:
			log.Printf("no leak: cluster %q no longer exists", suite.ClusterName())
		}
	}
	os.Exit(code)
}

func steps(t *testing.T) {
	t.Helper()
	if os.Getenv(EnvDemo) != "steps" {
		t.Skipf("only runs with %s=steps", EnvDemo)
	}
}

func cmKey(scenario string) client.ObjectKey {
	return client.ObjectKey{Namespace: "e2e-" + suite.Vars()["RUN_ID"] + "-" + scenario, Name: "managed"}
}

// A Check fails (wrong expected value).
func TestExpectFail_Check(t *testing.T) {
	steps(t)
	suite.Test(t, harness.ResourceTest{
		Name:          "failing check",
		Prerequisites: []string{"testdata/namespace-check.yaml"},
		Manifest:      "testdata/object-check.yaml",
		Created: func(ctx context.Context, t *testing.T, c client.Client, _ []*unstructured.Unstructured) {
			harness.Eventually(ctx, t, 10*time.Second, func(ctx context.Context) error {
				cm := &corev1.ConfigMap{}
				if err := c.Get(ctx, cmKey("check"), cm); err != nil {
					return err
				}
				if got := cm.Data["greeting"]; got != "WRONG" {
					return fmt.Errorf("greeting = %q, want %q", got, "WRONG")
				}
				return nil
			})
		},
	})
}

// The managed resource never becomes Ready and the wait times out.
func TestExpectFail_WaitTimeout(t *testing.T) {
	steps(t)
	suite.Test(t, harness.ResourceTest{
		Name:          "condition wait times out",
		Prerequisites: []string{"testdata/namespace-timeout.yaml"},
		Manifest:      "testdata/object-unready.yaml",
		Timeout:       20 * time.Second,
	})
}

// Applying the managed resource fails after its prerequisite was created.
func TestExpectFail_Create(t *testing.T) {
	steps(t)
	suite.Test(t, harness.ResourceTest{
		Name:          "apply rejected",
		Prerequisites: []string{"testdata/namespace-create.yaml"},
		Manifest:      "testdata/object-invalid.yaml",
	})
}

// The feature Setup step fails (a manifest that cannot be loaded).
func TestExpectFail_Setup(t *testing.T) {
	steps(t)
	suite.Test(t, harness.ResourceTest{
		Name:          "manifest missing",
		Prerequisites: []string{"testdata/namespace-setup.yaml"},
		Manifest:      "testdata/does-not-exist.yaml",
	})
}

// The Deleted check fails; the prerequisite must still be deleted.
func TestExpectFail_DeletedCheck(t *testing.T) {
	steps(t)
	suite.Test(t, harness.ResourceTest{
		Name:          "failing deleted check",
		Prerequisites: []string{"testdata/namespace-deleted.yaml"},
		Manifest:      "testdata/object-deleted.yaml",
		Deleted: func(ctx context.Context, t *testing.T, _ client.Client, _ []*unstructured.Unstructured) {
			harness.Eventually(ctx, t, 5*time.Second, func(context.Context) error {
				return errors.New("deliberately failing Deleted check")
			})
		},
	})
}

// A Check panics; without recovery this kills the test binary.
func TestExpectFail_Panic(t *testing.T) {
	steps(t)
	suite.Test(t, harness.ResourceTest{
		Name:          "panicking check",
		Prerequisites: []string{"testdata/namespace-panic.yaml"},
		Manifest:      "testdata/object-panic.yaml",
		Created: func(context.Context, *testing.T, client.Client, []*unstructured.Unstructured) {
			var m map[string]string
			m["boom"] = "assignment to nil map"
		},
	})
}

// TestCleanupVerified must run last (Go runs tests in source order). It
// checks that the scenarios above left no namespaces, Objects or ConfigMaps.
func TestCleanupVerified(t *testing.T) {
	steps(t)
	ctx := context.Background()
	r, err := suite.EnvConf().NewClient()
	if err != nil {
		t.Fatal(err)
	}
	c := r.Resources().GetControllerRuntimeClient()
	prefix := "e2e-" + suite.Vars()["RUN_ID"] + "-"

	nss := &corev1.NamespaceList{}
	if err := c.List(ctx, nss); err != nil {
		t.Fatal(err)
	}
	for _, ns := range nss.Items {
		if strings.HasPrefix(ns.Name, prefix) {
			t.Errorf("namespace %s left behind (phase %s)", ns.Name, ns.Status.Phase)
		}
	}

	objs := &unstructured.UnstructuredList{}
	objs.SetAPIVersion("kubernetes.m.crossplane.io/v1alpha1")
	objs.SetKind("ObjectList")
	if err := c.List(ctx, objs); err != nil {
		t.Fatal(err)
	}
	for _, o := range objs.Items {
		if strings.HasPrefix(o.GetNamespace(), prefix) {
			t.Errorf("Object %s/%s left behind", o.GetNamespace(), o.GetName())
		}
	}

	cms := &corev1.ConfigMapList{}
	if err := c.List(ctx, cms); err != nil {
		t.Fatal(err)
	}
	for _, cm := range cms.Items {
		if strings.HasPrefix(cm.Namespace, prefix) && cm.Name == "managed" {
			t.Errorf("external ConfigMap %s/%s left behind", cm.Namespace, cm.Name)
		}
	}
	t.Logf("no namespaces, Objects or ConfigMaps with prefix %s remain", prefix)
}
