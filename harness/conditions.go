package harness

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type condition struct {
	Type               string
	Status             string
	Reason             string
	Message            string
	ObservedGeneration int64
	HasGeneration      bool
}

func conditionsOf(u *unstructured.Unstructured) map[string]condition {
	out := map[string]condition{}
	list, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		c := condition{}
		c.Type, _ = m["type"].(string)
		c.Status, _ = m["status"].(string)
		c.Reason, _ = m["reason"].(string)
		c.Message, _ = m["message"].(string)
		if g, ok := m["observedGeneration"].(int64); ok {
			c.ObservedGeneration, c.HasGeneration = g, true
		}
		out[c.Type] = c
	}
	return out
}

// conditionsMet reports whether every wanted condition is True and, where the
// controller reports it, was observed for the object's current generation.
// The generation check stops an update from passing on stale conditions.
func conditionsMet(u *unstructured.Unstructured, want []string) bool {
	have := conditionsOf(u)
	for _, t := range want {
		c, ok := have[t]
		if !ok || c.Status != "True" {
			return false
		}
		if c.HasGeneration && c.ObservedGeneration != u.GetGeneration() {
			return false
		}
	}
	return true
}

// WaitForConditions waits until each object has all of the wanted conditions.
// On timeout the error includes the object's current conditions.
func WaitForConditions(ctx context.Context, c client.Client, objs []*unstructured.Unstructured, want []string, timeout time.Duration) error {
	for _, o := range objs {
		var live *unstructured.Unstructured
		err := poll(ctx, 2*time.Second, timeout, func(ctx context.Context) (bool, error) {
			l := o.DeepCopy()
			if err := c.Get(ctx, client.ObjectKeyFromObject(o), l); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			live = l
			return conditionsMet(live, want), nil
		})
		if err != nil {
			state := "  (object not found)"
			if live != nil {
				state = summarizeConditions(live, want)
			}
			return fmt.Errorf("%s did not reach %v: %w\n%s", describe(o), want, err, state)
		}
	}
	return nil
}

// summarizeConditions describes u's conditions, wanted ones first, noting any
// wanted condition the controller has not reported at all.
func summarizeConditions(u *unstructured.Unstructured, want []string) string {
	have := conditionsOf(u)
	types := slices.Clone(want)
	others := slices.Sorted(maps.Keys(have))
	for _, t := range others {
		if !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	if len(types) == 0 {
		return "  (no conditions reported)"
	}
	var b strings.Builder
	for _, t := range types {
		c, ok := have[t]
		if !ok {
			fmt.Fprintf(&b, "  %s: not reported\n", t)
			continue
		}
		fmt.Fprintf(&b, "  %s=%s reason=%s generation=%d/%d message=%q\n",
			c.Type, c.Status, c.Reason, c.ObservedGeneration, u.GetGeneration(), c.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}

// poll calls fn every interval until it reports done or returns an error, or
// until timeout. fn gets ctx rather than a context carrying the timeout, so
// the timeout is reported as such rather than as an error from whichever call
// it would have cut short (client-go's rate limiter fails a request early with
// "rate: Wait(n=1) would exceed context deadline").
func poll(ctx context.Context, interval, timeout time.Duration, fn wait.ConditionWithContextFunc) error {
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := wait.PollUntilContextCancel(pctx, interval, true, func(context.Context) (bool, error) {
		return fn(ctx)
	})
	if err != nil && ctx.Err() == nil && pctx.Err() != nil {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}
