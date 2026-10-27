package harness

import "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

// Labels the harness sets on objects it creates.
const (
	// LabelRunID is set to the run's RUN_ID on every object the harness
	// applies, so that everything a run created can be found (and cleaned up)
	// with a label selector, e.g. kubectl get managed -A -l xp-e2e/run-id=<id>.
	LabelRunID = "xp-e2e/run-id"
	// LabelCredentials marks Secrets the harness created from environment
	// variables (see SecretFromEnv).
	LabelCredentials = "xp-e2e/credentials"
)

// runLabels returns the labels for objects applied by the run vars belong to.
func runLabels(vars map[string]string) map[string]string {
	id := vars["RUN_ID"]
	if id == "" {
		return nil
	}
	return map[string]string{LabelRunID: id}
}

// addLabels merges labels into each object's existing labels. Labels already
// present in a manifest with the same key are overwritten.
func addLabels(objs []*unstructured.Unstructured, labels map[string]string) {
	if len(labels) == 0 {
		return
	}
	for _, o := range objs {
		l := o.GetLabels()
		if l == nil {
			l = make(map[string]string, len(labels))
		}
		for k, v := range labels {
			l[k] = v
		}
		o.SetLabels(l)
	}
}
