package harness

import (
	"context"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// Check verifies state outside the managed resources themselves, typically
// the external system the provider manages. objs holds the live managed
// resources (empty after deletion).
type Check func(ctx context.Context, t *testing.T, c client.Client, objs []*unstructured.Unstructured)

// Update re-applies changed managed resources and checks the result.
type Update struct {
	// Name describes the change, e.g. "change configmap data".
	Name string
	// Manifest holds the updated managed resources. It is server-side applied,
	// so it only needs the fields the test cares about.
	Manifest string
	Check    Check
}

// ResourceTest exercises managed resources through their whole lifecycle:
// create, wait for readiness, check the external system, update, delete and
// check the external system again.
type ResourceTest struct {
	Name string
	// Prerequisites are applied before Manifest and deleted after it, but are
	// not waited on for conditions (e.g. a namespace for the resources).
	Prerequisites []string
	// Manifest holds the managed resources under test.
	Manifest string
	// Conditions every managed resource must reach. Defaults to Ready, Synced.
	Conditions []string
	// Timeout for each wait. Defaults to 5 minutes.
	Timeout time.Duration

	Created Check
	Updates []Update
	Deleted Check
}

func (rt ResourceTest) feature(vars map[string]string) features.Feature {
	if len(rt.Conditions) == 0 {
		rt.Conditions = []string{"Ready", "Synced"}
	}
	if rt.Timeout == 0 {
		rt.Timeout = 5 * time.Minute
	}

	var prereqs, mrs []*unstructured.Unstructured

	b := features.New(rt.Name).
		Setup(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			var err error
			if prereqs, err = LoadAll(rt.Prerequisites, vars); err != nil {
				t.Fatal(err)
			}
			if mrs, err = LoadObjects(rt.Manifest, vars); err != nil {
				t.Fatal(err)
			}
			if err := Apply(ctx, crClient(t, cfg), append(append([]*unstructured.Unstructured{}, prereqs...), mrs...)); err != nil {
				t.Fatal(err)
			}
			return ctx
		}).
		Assess("managed resources become "+fmt.Sprint(rt.Conditions), func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := WaitForConditions(ctx, crClient(t, cfg), mrs, rt.Conditions, rt.Timeout); err != nil {
				t.Fatal(err)
			}
			return ctx
		})

	if rt.Created != nil {
		b = b.Assess("external state after create", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := crClient(t, cfg)
			rt.Created(ctx, t, c, live(ctx, t, c, mrs))
			return ctx
		})
	}

	for _, u := range rt.Updates {
		b = b.Assess("update: "+u.Name, func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := crClient(t, cfg)
			changed, err := LoadObjects(u.Manifest, vars)
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(ctx, c, changed); err != nil {
				t.Fatal(err)
			}
			if err := WaitForConditions(ctx, c, changed, rt.Conditions, rt.Timeout); err != nil {
				t.Fatal(err)
			}
			if u.Check != nil {
				u.Check(ctx, t, c, live(ctx, t, c, mrs))
			}
			return ctx
		})
	}

	// Teardown runs even when an assessment fails, so external resources are
	// cleaned up whenever the provider is able to.
	b = b.Teardown(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		c := crClient(t, cfg)
		if err := Delete(ctx, c, mrs); err != nil {
			t.Error(err)
		}
		if err := WaitForDeletion(ctx, c, mrs, rt.Timeout); err != nil {
			t.Error(err)
		} else if rt.Deleted != nil {
			rt.Deleted(ctx, t, c, nil)
		}
		if err := Delete(ctx, c, prereqs); err != nil {
			t.Error(err)
		}
		return ctx
	})

	return b.Feature()
}

func crClient(t *testing.T, cfg *envconf.Config) client.Client {
	t.Helper()
	r, err := cfg.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	return r.Resources().GetControllerRuntimeClient()
}

func live(ctx context.Context, t *testing.T, c client.Client, objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	t.Helper()
	out := make([]*unstructured.Unstructured, 0, len(objs))
	for _, o := range objs {
		l := o.DeepCopy()
		if err := c.Get(ctx, client.ObjectKeyFromObject(o), l); err != nil {
			t.Fatalf("getting %s: %v", describe(o), err)
		}
		out = append(out, l)
	}
	return out
}

// Eventually polls fn until it returns nil or timeout elapses, then fails the
// test with fn's last error. Checks use it because external systems are
// often eventually consistent.
func Eventually(ctx context.Context, t *testing.T, timeout time.Duration, fn func(ctx context.Context) error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := fn(ctx)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s: %v", timeout, err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context done: %v (last error: %v)", ctx.Err(), err)
		case <-time.After(2 * time.Second):
		}
	}
}
