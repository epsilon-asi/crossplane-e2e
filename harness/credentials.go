package harness

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SecretFromEnv creates one key of a Secret from environment variables, for
// providers whose ProviderConfig reads credentials from a Secret. Secrets are
// applied after a provider's PreInstall manifests and before the provider
// package and its PostInstall manifests (ProviderConfigs).
//
// The variables in Env are also requirements of the whole suite: if any is
// unset the suite is skipped without creating a cluster (see EnvStrict).
//
// Values are only ever sent to the test cluster. They are not logged, written
// to disk by the harness, or included in error messages. Note that a kept
// cluster (XP_E2E_CLUSTER / XP_E2E_KEEP) still holds them in its datastore
// until the harness deletes them at the end of a run whose managed resources
// were all cleaned up.
type SecretFromEnv struct {
	// Namespace of the Secret. Defaults to crossplane-system.
	Namespace string
	// Name of the Secret. Several SecretFromEnv with the same Namespace and
	// Name become keys of one Secret.
	Name string
	// Key within the Secret. Defaults to "credentials".
	Key string
	// Env lists the environment variables the value is built from.
	Env []string
	// Template renders the value with text/template, with the values of Env
	// as data (e.g. {{ .ACME_TOKEN }}). The function json encodes a value as
	// a JSON string, quotes included:
	//
	//	{"token": {{ json .ACME_TOKEN }}}
	//
	// When empty, Env must have exactly one entry, used verbatim.
	Template string
}

func (s SecretFromEnv) withDefaults() SecretFromEnv {
	if s.Namespace == "" {
		s.Namespace = DefaultCrossplaneNS
	}
	if s.Key == "" {
		s.Key = "credentials"
	}
	return s
}

func (s SecretFromEnv) String() string {
	s = s.withDefaults()
	return fmt.Sprintf("Secret %s/%s key %s", s.Namespace, s.Name, s.Key)
}

// value renders the Secret value. It returns the secret inputs too, so the
// caller can redact them from any later error.
func (s SecretFromEnv) value(lookup func(string) (string, bool)) (value []byte, inputs []string, err error) {
	if s.Name == "" || len(s.Env) == 0 {
		return nil, nil, fmt.Errorf("%s: Name and Env are required", s)
	}
	if missing := missingEnv(lookup, s.Env); len(missing) > 0 {
		return nil, nil, fmt.Errorf("%s: environment variables not set: %s", s, strings.Join(missing, ", "))
	}
	data := make(map[string]string, len(s.Env))
	for _, e := range s.Env {
		data[e], _ = lookup(e)
		inputs = append(inputs, data[e])
	}
	if s.Template == "" {
		if len(s.Env) != 1 {
			return nil, inputs, fmt.Errorf("%s: Template is required when Env has more than one variable", s)
		}
		return []byte(data[s.Env[0]]), inputs, nil
	}
	tmpl, err := template.New(s.String()).Option("missingkey=error").Funcs(template.FuncMap{
		"json": func(v string) (string, error) {
			b, err := json.Marshal(v)
			return string(b), err
		},
	}).Parse(s.Template)
	if err != nil {
		// Parse errors only quote the template, which holds no secret values.
		return nil, inputs, fmt.Errorf("%s: parsing template: %w", s, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, inputs, redact(fmt.Errorf("%s: rendering template: %w", s, err), inputs)
	}
	return buf.Bytes(), inputs, nil
}

// credentialSecrets builds the Secrets for creds, returning them together
// with every secret string (inputs and rendered values) for redaction.
func credentialSecrets(creds []SecretFromEnv, lookup func(string) (string, bool), labels map[string]string) ([]*unstructured.Unstructured, []string, error) {
	type ref struct{ ns, name string }
	data := map[ref]map[string]any{}
	var order []ref
	var secrets []string
	for _, c := range creds {
		c = c.withDefaults()
		v, inputs, err := c.value(lookup)
		secrets = append(secrets, inputs...)
		if err != nil {
			return nil, secrets, err
		}
		secrets = append(secrets, string(v))
		r := ref{c.Namespace, c.Name}
		if data[r] == nil {
			data[r] = map[string]any{}
			order = append(order, r)
		}
		if _, dup := data[r][c.Key]; dup {
			return nil, secrets, fmt.Errorf("%s: key declared twice", c)
		}
		data[r][c.Key] = base64.StdEncoding.EncodeToString(v)
	}
	objs := make([]*unstructured.Unstructured, 0, len(order))
	for _, r := range order {
		objs = append(objs, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": r.name, "namespace": r.ns},
			"type":       "Opaque",
			"data":       data[r],
		}})
	}
	all := map[string]string{LabelCredentials: "true"}
	for k, v := range labels {
		all[k] = v
	}
	addLabels(objs, all)
	return objs, secrets, nil
}

// applyCredentials creates the Secrets declared by creds. Errors are redacted.
func applyCredentials(ctx context.Context, c client.Client, creds []SecretFromEnv, lookup func(string) (string, bool), labels map[string]string) error {
	if len(creds) == 0 {
		return nil
	}
	objs, secrets, err := credentialSecrets(creds, lookup, labels)
	if err != nil {
		return redact(err, secrets)
	}
	return redact(Apply(ctx, c, objs), secrets)
}

// redact returns err with every occurrence of a secret value, raw or base64
// encoded, replaced. The original error is dropped (not wrapped) so that
// nothing can recover the unredacted message through errors.Unwrap.
func redact(err error, secrets []string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	var olds []string
	for _, s := range secrets {
		if s == "" {
			continue
		}
		olds = append(olds, s, base64.StdEncoding.EncodeToString([]byte(s)))
		if b, jerr := json.Marshal(s); jerr == nil {
			olds = append(olds, strings.Trim(string(b), `"`))
		}
	}
	// Longest first, so a secret that contains another is replaced whole.
	sort.Slice(olds, func(i, j int) bool { return len(olds[i]) > len(olds[j]) })
	for _, o := range olds {
		msg = strings.ReplaceAll(msg, o, "[REDACTED]")
	}
	return errors.New(msg)
}
