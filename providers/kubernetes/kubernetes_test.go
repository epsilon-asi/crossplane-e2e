// Package kubernetes tests crossplane-contrib/provider-kubernetes. The
// "external system" is the test cluster itself, so every check can be made
// directly with the Kubernetes API and no credentials are needed.
package kubernetes

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"crossplane-test-environment/harness"
)

var suite *harness.Suite

func TestMain(m *testing.M) {
	suite = harness.NewSuite(harness.Config{
		Providers: []harness.Provider{{
			Name:              "provider-kubernetes",
			Package:           "xpkg.crossplane.io/crossplane-contrib/provider-kubernetes:v1.4.1",
			RuntimeConfigName: "provider-kubernetes",
			PreInstall:        []string{"testdata/setup/runtime.yaml"},
			PostInstall:       []string{"testdata/setup/providerconfig.yaml"},
		}},
	})
	os.Exit(suite.Run(m))
}

func TestObjectConfigMap(t *testing.T) {
	ns := "e2e-" + suite.Vars()["RUN_ID"]
	key := client.ObjectKey{Namespace: ns, Name: "managed"}

	suite.Test(t, harness.ResourceTest{
		Name:          "Object manages a ConfigMap",
		Prerequisites: []string{"testdata/configmap/namespace.yaml"},
		Manifest:      "testdata/configmap/object.yaml",
		Created: func(ctx context.Context, t *testing.T, c client.Client, _ []*unstructured.Unstructured) {
			expectGreeting(ctx, t, c, key, "hello")
		},
		Updates: []harness.Update{{
			Name:     "change ConfigMap data",
			Manifest: "testdata/configmap/object-updated.yaml",
			Check: func(ctx context.Context, t *testing.T, c client.Client, _ []*unstructured.Unstructured) {
				expectGreeting(ctx, t, c, key, "goodbye")
			},
		}},
		Deleted: func(ctx context.Context, t *testing.T, c client.Client, _ []*unstructured.Unstructured) {
			harness.Eventually(ctx, t, time.Minute, func(ctx context.Context) error {
				err := c.Get(ctx, key, &corev1.ConfigMap{})
				if kerrors.IsNotFound(err) {
					return nil
				}
				return fmt.Errorf("ConfigMap %s still exists (err=%v)", key, err)
			})
		},
	})
}

func expectGreeting(ctx context.Context, t *testing.T, c client.Client, key client.ObjectKey, want string) {
	t.Helper()
	harness.Eventually(ctx, t, time.Minute, func(ctx context.Context) error {
		cm := &corev1.ConfigMap{}
		if err := c.Get(ctx, key, cm); err != nil {
			return err
		}
		if got := cm.Data["greeting"]; got != want {
			return fmt.Errorf("ConfigMap %s greeting = %q, want %q", key, got, want)
		}
		return nil
	})
}
