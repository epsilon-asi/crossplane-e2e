package harness

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

const token = `s3cr"et\tok`

func TestSecretFromEnvValue(t *testing.T) {
	env := lookupFrom(map[string]string{"TOKEN": token, "USER": "bob", "EMPTY": ""})
	cases := map[string]struct {
		s       SecretFromEnv
		want    string
		wantErr string
	}{
		"verbatim":        {s: SecretFromEnv{Name: "c", Env: []string{"TOKEN"}}, want: token},
		"json template":   {s: SecretFromEnv{Name: "c", Env: []string{"TOKEN"}, Template: `{"token": {{ json .TOKEN }}}`}, want: `{"token": "s3cr\"et\\tok"}`},
		"two vars":        {s: SecretFromEnv{Name: "c", Env: []string{"USER", "TOKEN"}, Template: `{{ .USER }}:{{ .TOKEN }}`}, want: "bob:" + token},
		"missing var":     {s: SecretFromEnv{Name: "c", Env: []string{"NOPE"}}, wantErr: "not set: NOPE"},
		"empty var":       {s: SecretFromEnv{Name: "c", Env: []string{"EMPTY"}}, wantErr: "not set: EMPTY"},
		"two no template": {s: SecretFromEnv{Name: "c", Env: []string{"USER", "TOKEN"}}, wantErr: "Template is required"},
		"undeclared ref":  {s: SecretFromEnv{Name: "c", Env: []string{"TOKEN"}, Template: `{{ .OTHER }}`}, wantErr: "OTHER"},
		"no name":         {s: SecretFromEnv{Env: []string{"TOKEN"}}, wantErr: "required"},
	}
	for name, tc := range cases {
		got, _, err := tc.s.value(env)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want containing %q", name, err, tc.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), token) {
				t.Errorf("%s: error leaks the secret: %v", name, err)
			}
			continue
		}
		if err != nil || string(got) != tc.want {
			t.Errorf("%s: value = %q, %v; want %q", name, got, err, tc.want)
		}
	}
}

func TestCredentialSecrets(t *testing.T) {
	env := lookupFrom(map[string]string{"A": "aaa", "B": "bbb"})
	objs, secrets, err := credentialSecrets([]SecretFromEnv{
		{Name: "creds", Key: "a", Env: []string{"A"}},
		{Name: "creds", Key: "b", Env: []string{"B"}, Template: `x{{ .B }}`},
		{Namespace: "other", Name: "creds", Env: []string{"A"}},
	}, env, map[string]string{LabelRunID: "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 {
		t.Fatalf("got %d Secrets, want 2 (keys of one Secret are merged)", len(objs))
	}
	s := objs[0]
	if s.GetNamespace() != DefaultCrossplaneNS || s.GetName() != "creds" {
		t.Errorf("Secret = %s, want crossplane-system/creds", describe(s))
	}
	data, _, _ := unstructured.NestedStringMap(s.Object, "data")
	want := map[string]string{"a": b64("aaa"), "b": b64("xbbb")}
	if !reflect.DeepEqual(data, want) {
		t.Errorf("data = %v, want %v", data, want)
	}
	if l := s.GetLabels(); l[LabelRunID] != "abc123" || l[LabelCredentials] != "true" {
		t.Errorf("labels = %v", l)
	}
	if objs[1].GetNamespace() != "other" {
		t.Errorf("second Secret = %s, want namespace other", describe(objs[1]))
	}
	for _, v := range []string{"aaa", "bbb", "xbbb"} {
		if !contains(secrets, v) {
			t.Errorf("secrets for redaction %v missing %q", secrets, v)
		}
	}

	if _, _, err := credentialSecrets([]SecretFromEnv{
		{Name: "creds", Env: []string{"A"}}, {Name: "creds", Env: []string{"B"}},
	}, env, nil); err == nil {
		t.Error("duplicate key: want error")
	}
}

func TestRedact(t *testing.T) {
	err := errors.New(`apply failed: data "` + b64(token) + `" raw ` + token + ` json s3cr\"et\\tok`)
	got := redact(err, []string{token, ""}).Error()
	if strings.Contains(got, "s3cr") || strings.Contains(got, b64(token)) {
		t.Errorf("redact = %q, still contains the secret", got)
	}
	if !strings.Contains(got, "apply failed") || strings.Count(got, "[REDACTED]") != 3 {
		t.Errorf("redact = %q, want three redactions and the rest kept", got)
	}
	if redact(nil, []string{token}) != nil {
		t.Error("redact(nil) != nil")
	}
}

func TestRequirementsCheck(t *testing.T) {
	cases := map[string]struct {
		req     Requirements
		env     map[string]string
		want    verdict
		mention string
	}{
		"nothing required":       {Requirements{}, nil, run, ""},
		"env present":            {Requirements{Env: []string{"T"}}, map[string]string{"T": "x"}, run, ""},
		"env missing":            {Requirements{Env: []string{"T", "U"}}, map[string]string{"T": "x"}, skip, "not set: U"},
		"env empty":              {Requirements{Env: []string{"T"}}, map[string]string{"T": ""}, skip, "not set: T"},
		"env missing strict":     {Requirements{Env: []string{"T"}}, map[string]string{EnvStrict: "true"}, fail, "not set: T"},
		"paid not opted in":      {Requirements{Paid: true}, nil, skip, EnvPaid + "=true"},
		"paid invalid value":     {Requirements{Paid: true}, map[string]string{EnvPaid: "yes please"}, skip, EnvPaid},
		"paid opted in":          {Requirements{Paid: true}, map[string]string{EnvPaid: "true"}, run, ""},
		"paid strict no opt-in":  {Requirements{Paid: true, Env: []string{"T"}}, map[string]string{EnvStrict: "1"}, skip, EnvPaid},
		"paid opted in, no env":  {Requirements{Paid: true, Env: []string{"T"}}, map[string]string{EnvPaid: "1"}, skip, "not set: T"},
		"paid opted in, env set": {Requirements{Paid: true, Env: []string{"T"}}, map[string]string{EnvPaid: "1", "T": "x"}, run, ""},
	}
	for name, tc := range cases {
		got, reason := tc.req.check(lookupFrom(tc.env))
		if got != tc.want || !strings.Contains(reason, tc.mention) {
			t.Errorf("%s: check = %d %q, want %d mentioning %q", name, got, reason, tc.want, tc.mention)
		}
		if tc.env["T"] != "" && strings.Contains(reason, tc.env["T"]) {
			t.Errorf("%s: reason leaks a value: %q", name, reason)
		}
	}
}

func TestRequirementsMerge(t *testing.T) {
	got := Requirements{Env: []string{"A", "B"}}.merge(Requirements{Env: []string{"B", "C"}, Paid: true})
	if want := (Requirements{Env: []string{"A", "B", "C"}, Paid: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("merge = %+v, want %+v", got, want)
	}
}

func TestSuiteRequirementsIncludeCredentials(t *testing.T) {
	s := &Suite{cfg: Config{
		Requires:  Requirements{Env: []string{"X"}},
		Providers: []Provider{{Credentials: []SecretFromEnv{{Name: "c", Env: []string{"TOKEN", "X"}}}}},
	}}
	if got := s.requirements().Env; !reflect.DeepEqual(got, []string{"X", "TOKEN"}) {
		t.Errorf("requirements = %v, want [X TOKEN]", got)
	}
}

func TestLoadObjectsAddsRunLabel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	data := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n  labels:\n    keep: me\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, err := LoadObjects(path, map[string]string{"RUN_ID": "abc123"})
	if err != nil {
		t.Fatal(err)
	}
	if got := objs[0].GetLabels(); !reflect.DeepEqual(got, map[string]string{"keep": "me", LabelRunID: "abc123"}) {
		t.Errorf("labels[0] = %v", got)
	}
	if got := objs[1].GetLabels(); !reflect.DeepEqual(got, map[string]string{LabelRunID: "abc123"}) {
		t.Errorf("labels[1] = %v", got)
	}

	objs, err = LoadObjects(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := objs[1].GetLabels()[LabelRunID]; ok {
		t.Error("no RUN_ID: want no run label")
	}
}

func TestSweepScopeMatches(t *testing.T) {
	run := SweepScope{RunID: "abc123"}
	all := SweepScope{}
	cases := []struct {
		name     string
		run, all bool
	}{
		{"e2e-abc123", true, true},
		{"e2e-abc123-bucket", true, true},
		{"e2e-bucket-abc123", true, true},
		{"E2E-ABC123_db", true, true},
		{"e2e-abc1234", false, true},
		{"e2e-xabc123", false, true},
		{"e2e-def456-bucket", false, true},
		{"prod-abc123", false, false},
		{"bucket-e2e-abc123", false, false},
	}
	for _, c := range cases {
		if got := run.Matches(c.name); got != c.run {
			t.Errorf("run scope Matches(%q) = %t, want %t", c.name, got, c.run)
		}
		if got := all.Matches(c.name); got != c.all {
			t.Errorf("all scope Matches(%q) = %t, want %t", c.name, got, c.all)
		}
	}
}

func TestParseSweepScope(t *testing.T) {
	if s, err := parseSweepScope("all"); err != nil || s.RunID != "" {
		t.Errorf("all: %+v, %v", s, err)
	}
	if s, err := parseSweepScope("0a1b2c"); err != nil || s.RunID != "0a1b2c" {
		t.Errorf("run id: %+v, %v", s, err)
	}
	for _, v := range []string{"true", "1", "ABC123", "abc12", "e2e-abc123"} {
		if _, err := parseSweepScope(v); err == nil {
			t.Errorf("%q: want error", v)
		}
	}
}

func TestSelectSweepers(t *testing.T) {
	noop := func(context.Context, SweepScope) error { return nil }
	sw := func(name string, deps ...string) Sweeper {
		return Sweeper{Name: name, Dependencies: deps, Sweep: noop}
	}
	all := []Sweeper{sw("networks", "instances"), sw("instances", "dns"), sw("dns"), sw("buckets")}
	names := func(ss []Sweeper) []string {
		var out []string
		for _, s := range ss {
			out = append(out, s.Name)
		}
		return out
	}
	cases := map[string]struct {
		all     []Sweeper
		filter  string
		want    []string
		wantErr string
	}{
		"all, deps first":   {all: all, want: []string{"dns", "instances", "networks", "buckets"}},
		"filter with deps":  {all: all, filter: "networks", want: []string{"dns", "instances", "networks"}},
		"filter list":       {all: all, filter: " buckets, dns ", want: []string{"buckets", "dns"}},
		"unknown":           {all: all, filter: "vms", wantErr: `unknown sweeper "vms"`},
		"unknown dep":       {all: []Sweeper{sw("a", "b")}, wantErr: `unknown sweeper "b"`},
		"cycle":             {all: []Sweeper{sw("a", "b"), sw("b", "a")}, wantErr: "cycle: a -> b -> a"},
		"duplicate":         {all: []Sweeper{sw("a"), sw("a")}, wantErr: "declared twice"},
		"missing func":      {all: []Sweeper{{Name: "a"}}, wantErr: "required"},
		"no sweepers":       {all: nil, want: nil},
		"empty filter item": {all: all, filter: ",", want: []string{"dns", "instances", "networks", "buckets"}},
	}
	for name, tc := range cases {
		got, err := selectSweepers(tc.all, tc.filter)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: err = %v, want %q", name, err, tc.wantErr)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(names(got), tc.want) {
			t.Errorf("%s: selected %v, %v; want %v", name, names(got), err, tc.want)
		}
	}
}

func TestRunSweepers(t *testing.T) {
	var ran []string
	mk := func(name string, err error, requires ...string) Sweeper {
		return Sweeper{Name: name, Requires: requires, Sweep: func(_ context.Context, s SweepScope) error {
			ran = append(ran, name+":"+s.RunID)
			return err
		}}
	}
	sweepers := []Sweeper{mk("a", nil), mk("b", errors.New("boom")), mk("c", nil, "CREDS"), mk("d", nil)}

	err := runSweepers(context.Background(), sweepers, SweepScope{RunID: "abc123"}, lookupFrom(nil), false)
	if want := []string{"a:abc123", "b:abc123", "d:abc123"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %v, want %v (c skipped, d still runs after b fails)", ran, want)
	}
	if err == nil || !strings.Contains(err.Error(), "sweeper b: boom") || strings.Contains(err.Error(), "sweeper c") {
		t.Errorf("err = %v, want only b's failure", err)
	}

	ran = nil
	err = runSweepers(context.Background(), sweepers[2:3], SweepScope{}, lookupFrom(nil), true)
	if err == nil || !strings.Contains(err.Error(), "CREDS") || len(ran) != 0 {
		t.Errorf("strict: err = %v, ran = %v; want missing CREDS error and nothing run", err, ran)
	}
}

func TestGuardAfter(t *testing.T) {
	cases := []struct{ timeout, budget, want time.Duration }{
		{0, 7 * time.Minute, 0},
		{20 * time.Minute, 7 * time.Minute, 13 * time.Minute},
		{10 * time.Minute, 7 * time.Minute, 5 * time.Minute},
		{time.Minute, 7 * time.Minute, 30 * time.Second},
	}
	for _, c := range cases {
		if got := guardAfter(c.timeout, c.budget); got != c.want {
			t.Errorf("guardAfter(%s, %s) = %s, want %s", c.timeout, c.budget, got, c.want)
		}
	}
}

func TestOrphans(t *testing.T) {
	mr := func(spec map[string]any) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	}
	cases := map[string]struct {
		spec map[string]any
		want bool
	}{
		"defaults":           {map[string]any{}, false},
		"delete policy":      {map[string]any{"deletionPolicy": "Delete"}, false},
		"orphan policy":      {map[string]any{"deletionPolicy": "Orphan"}, true},
		"all policies":       {map[string]any{"managementPolicies": []any{"*"}}, false},
		"observe only":       {map[string]any{"managementPolicies": []any{"Observe"}}, true},
		"explicit delete":    {map[string]any{"managementPolicies": []any{"Observe", "Create", "Delete"}}, false},
		"no delete in list":  {map[string]any{"managementPolicies": []any{"Observe", "Create", "Update"}}, true},
		"empty policies set": {map[string]any{"managementPolicies": []any{}}, false},
	}
	for name, tc := range cases {
		if got := orphans(mr(tc.spec)); got != tc.want {
			t.Errorf("%s: orphans = %t, want %t", name, got, tc.want)
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
