package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"time"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const fieldOwner = "xp-e2e"

var varRef = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// render replaces ${NAME} references with vars. Unknown references are an
// error so a typo can't silently create a resource with a literal "${...}".
func render(data []byte, vars map[string]string) ([]byte, error) {
	var missing []string
	out := varRef.ReplaceAllFunc(data, func(m []byte) []byte {
		name := string(varRef.FindSubmatch(m)[1])
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
			return m
		}
		return []byte(v)
	})
	if len(missing) > 0 {
		return nil, fmt.Errorf("undefined variables %v", missing)
	}
	return out, nil
}

// LoadObjects reads a (multi-document) YAML file, renders vars into it and
// decodes every non-empty document. When vars has a RUN_ID every object is
// labelled with LabelRunID so the run's objects can be found later.
func LoadObjects(path string, vars map[string]string) ([]*unstructured.Unstructured, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	data, err := render(raw, vars)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var objs []*unstructured.Unstructured
	dec := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		u := &unstructured.Unstructured{}
		if err := dec.Decode(&u.Object); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		objs = append(objs, u)
	}
	addLabels(objs, runLabels(vars))
	return objs, nil
}

// LoadAll loads several manifest files in order.
func LoadAll(paths []string, vars map[string]string) ([]*unstructured.Unstructured, error) {
	var objs []*unstructured.Unstructured
	for _, p := range paths {
		o, err := LoadObjects(p, vars)
		if err != nil {
			return nil, err
		}
		objs = append(objs, o...)
	}
	return objs, nil
}

// Apply server-side applies objs in order. Kinds whose CRDs were only just
// installed (e.g. a ProviderConfig right after its provider) are retried
// until the API server serves them.
func Apply(ctx context.Context, c client.Client, objs []*unstructured.Unstructured) error {
	for _, o := range objs {
		err := wait.PollUntilContextTimeout(ctx, time.Second, time.Minute, true, func(ctx context.Context) (bool, error) {
			ac := client.ApplyConfigurationFromUnstructured(o.DeepCopy())
			err := c.Apply(ctx, ac, client.FieldOwner(fieldOwner), client.ForceOwnership)
			if meta.IsNoMatchError(err) {
				return false, nil
			}
			return err == nil, err
		})
		if err != nil {
			return fmt.Errorf("applying %s: %w", describe(o), err)
		}
	}
	return nil
}

// Delete deletes objs in reverse order, ignoring ones that are already gone.
func Delete(ctx context.Context, c client.Client, objs []*unstructured.Unstructured) error {
	var errs []error
	for i := len(objs) - 1; i >= 0; i-- {
		if err := c.Delete(ctx, objs[i].DeepCopy()); err != nil && !kerrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
			errs = append(errs, fmt.Errorf("deleting %s: %w", describe(objs[i]), err))
		}
	}
	return errors.Join(errs...)
}

// WaitForDeletion waits until none of objs exist any more. For managed
// resources this means the provider has deleted the external resource and
// removed its finalizer.
func WaitForDeletion(ctx context.Context, c client.Client, objs []*unstructured.Unstructured, timeout time.Duration) error {
	for _, o := range objs {
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
			err := c.Get(ctx, client.ObjectKeyFromObject(o), o.DeepCopy())
			if kerrors.IsNotFound(err) || meta.IsNoMatchError(err) {
				return true, nil
			}
			return false, err
		})
		if err != nil {
			return fmt.Errorf("waiting for %s to be deleted: %w", describe(o), err)
		}
	}
	return nil
}

func describe(o *unstructured.Unstructured) string {
	name := o.GetName()
	if ns := o.GetNamespace(); ns != "" {
		name = ns + "/" + name
	}
	return fmt.Sprintf("%s %s", o.GroupVersionKind().Kind, name)
}
