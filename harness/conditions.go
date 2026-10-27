package harness

import (
	"context"
	"fmt"
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
		live := o.DeepCopy()
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, client.ObjectKeyFromObject(o), live); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			return conditionsMet(live, want), nil
		})
		if err != nil {
			return fmt.Errorf("%s did not reach %v: %w\n%s", describe(o), want, err, summarizeConditions(live))
		}
	}
	return nil
}

func summarizeConditions(u *unstructured.Unstructured) string {
	have := conditionsOf(u)
	if len(have) == 0 {
		return "  (no conditions reported)"
	}
	var b strings.Builder
	for _, c := range have {
		fmt.Fprintf(&b, "  %s=%s reason=%s generation=%d/%d message=%q\n",
			c.Type, c.Status, c.Reason, c.ObservedGeneration, u.GetGeneration(), c.Message)
	}
	return strings.TrimRight(b.String(), "\n")
}
