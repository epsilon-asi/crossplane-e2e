package harness

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRender(t *testing.T) {
	got, err := render([]byte("name: e2e-${RUN_ID} # $notAVar ${ RUN_ID }"), map[string]string{"RUN_ID": "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "name: e2e-abc # $notAVar ${ RUN_ID }"; string(got) != want {
		t.Errorf("render = %q, want %q", got, want)
	}
	if _, err := render([]byte("${MISSING}"), nil); err == nil {
		t.Error("render with undefined variable: want error")
	}
}

func TestLoadObjectsSkipsEmptyDocuments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.yaml")
	data := "---\n# comment only\n---\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: ns-${RUN_ID}\n---\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	objs, err := LoadObjects(path, map[string]string{"RUN_ID": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].GetName() != "ns-x" {
		t.Fatalf("LoadObjects = %v, want one Namespace ns-x", objs)
	}
}

func TestConditionsMet(t *testing.T) {
	obj := func(gen int64, conds ...map[string]any) *unstructured.Unstructured {
		list := make([]any, len(conds))
		for i, c := range conds {
			list[i] = c
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"generation": gen},
			"status":   map[string]any{"conditions": list},
		}}
	}
	ready := map[string]any{"type": "Ready", "status": "True"}
	syncedGen1 := map[string]any{"type": "Synced", "status": "True", "observedGeneration": int64(1)}

	cases := map[string]struct {
		u    *unstructured.Unstructured
		want bool
	}{
		"all true":             {obj(1, ready, syncedGen1), true},
		"missing condition":    {obj(1, ready), false},
		"false condition":      {obj(1, ready, map[string]any{"type": "Synced", "status": "False"}), false},
		"stale generation":     {obj(2, ready, syncedGen1), false},
		"no generation report": {obj(5, ready, map[string]any{"type": "Synced", "status": "True"}), true},
	}
	for name, tc := range cases {
		if got := conditionsMet(tc.u, []string{"Ready", "Synced"}); got != tc.want {
			t.Errorf("%s: conditionsMet = %t, want %t", name, got, tc.want)
		}
	}
}
