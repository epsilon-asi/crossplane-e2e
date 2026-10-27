package harness

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
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
// check the external system again. Each of those is its own step in the
// `go test -v` output; cleanup also runs, as a safety net, when a step fails.
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

	// prereqs and mrs are loaded in Setup. applied tracks every managed
	// resource handed to the API server (including any an update adds), so
	// cleanup deletes all of them.
	var prereqs, mrs, applied []*unstructured.Unstructured
	deleteAttempted := false

	// Setup only loads manifests and has no side effects: when a Setup step
	// fails, e2e-framework skips the assessments *and* Teardown, so nothing
	// Setup created would be cleaned up.
	b := features.New(rt.Name).
		Setup(guard(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			var err error
			if prereqs, err = LoadAll(rt.Prerequisites, vars); err != nil {
				t.Fatal(err)
			}
			if mrs, err = LoadObjects(rt.Manifest, vars); err != nil {
				t.Fatal(err)
			}
			return ctx
		})).
		Assess("create", guard(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := crClient(t, cfg)
			if err := Apply(ctx, c, prereqs); err != nil {
				t.Fatal(err)
			}
			applied = track(applied, mrs...)
			if err := Apply(ctx, c, mrs); err != nil {
				t.Fatal(err)
			}
			return ctx
		})).
		Assess("managed resources become "+fmt.Sprint(rt.Conditions), guard(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			if err := WaitForConditions(ctx, crClient(t, cfg), mrs, rt.Conditions, rt.Timeout); err != nil {
				t.Fatal(err)
			}
			return ctx
		}))

	if rt.Created != nil {
		b = b.Assess("external state after create", guard(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := crClient(t, cfg)
			rt.Created(ctx, t, c, live(ctx, t, c, mrs))
			return ctx
		}))
	}

	for _, u := range rt.Updates {
		b = b.Assess("update: "+u.Name, guard(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
			c := crClient(t, cfg)
			changed, err := LoadObjects(u.Manifest, vars)
			if err != nil {
				t.Fatal(err)
			}
			applied = track(applied, changed...)
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
		}))
	}

	deleteStep := "delete"
	if rt.Deleted != nil {
		deleteStep = "delete and verify external removal"
	}
	b = b.Assess(deleteStep, guard(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		c := crClient(t, cfg)
		deleteAttempted = true
		if err := Delete(ctx, c, applied); err != nil {
			t.Fatal(err)
		}
		if err := WaitForDeletion(ctx, c, applied, rt.Timeout); err != nil {
			t.Fatal(err)
		}
		if rt.Deleted != nil {
			rt.Deleted(ctx, t, c, nil)
		}
		return ctx
	}))

	// Teardown is the safety net for when a step failed before or during
	// deletion. e2e-framework runs it even when an assessment fails, so it
	// must be idempotent, and it must not stop at its first error.
	b = b.Teardown(guard(func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		r, err := cfg.NewClient()
		if err != nil {
			t.Errorf("cleanup: %v", err)
			return ctx
		}
		c := r.Resources().GetControllerRuntimeClient()
		if err := Delete(ctx, c, applied); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		// The delete step has already waited (and reported) if it ran.
		if !deleteAttempted {
			if err := WaitForDeletion(ctx, c, applied, rt.Timeout); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		if err := Delete(ctx, c, prereqs); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		if err := WaitForDeletion(ctx, c, prereqs, rt.Timeout); err != nil {
			t.Errorf("cleanup: %v", err)
		}
		return ctx
	}))

	return b.Feature()
}

// guard turns a panic in a step (typically in a provider's Check) into a test
// failure. Otherwise a panic in a test goroutine kills the test binary before
// Teardown or the suite's Finish actions run, leaking the cluster.
func guard(fn features.Func) features.Func {
	return func(ctx context.Context, t *testing.T, cfg *envconf.Config) (out context.Context) {
		t.Helper()
		out = ctx
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic: %v\n%s", r, debug.Stack())
			}
		}()
		return fn(ctx, t, cfg)
	}
}

// track appends objs to list, skipping objects it already holds.
func track(list []*unstructured.Unstructured, objs ...*unstructured.Unstructured) []*unstructured.Unstructured {
	for _, o := range objs {
		if !slices.ContainsFunc(list, func(l *unstructured.Unstructured) bool { return sameObject(l, o) }) {
			list = append(list, o)
		}
	}
	return list
}

func sameObject(a, b *unstructured.Unstructured) bool {
	return a.GroupVersionKind().GroupKind() == b.GroupVersionKind().GroupKind() &&
		a.GetNamespace() == b.GetNamespace() && a.GetName() == b.GetName()
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
