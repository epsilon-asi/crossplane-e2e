package harness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestListHasClusterIsExact(t *testing.T) {
	list := []byte(`[{"name":"xp-test","nodes":[]},{"name":"xp-reuse-check","nodes":[]}]`)
	cases := map[string]bool{
		"xp-test":        true,
		"xp-reuse-check": true,
		"xp":             false, // a prefix of existing names, not a cluster
		"xp-reuse":       false,
		"xp-test-2":      false,
	}
	for name, want := range cases {
		got, err := listHasCluster(list, name)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("listHasCluster(%q) = %t, want %t", name, got, want)
		}
	}
	if got, err := listHasCluster([]byte("[]"), "xp"); err != nil || got {
		t.Errorf("empty list: got %t, %v", got, err)
	}
}

// fakeK3d puts a k3d on PATH that lists the given clusters and fails to delete
// any of them.
func fakeK3d(t *testing.T, clusters ...string) {
	t.Helper()
	var names []string
	for _, c := range clusters {
		names = append(names, `{"name":"`+c+`"}`)
	}
	script := "#!/bin/sh\n" +
		"case \"$1 $2\" in\n" +
		"  'cluster list') echo '[" + strings.Join(names, ",") + "]' ;;\n" +
		"  'cluster delete') echo 'simulated delete failure' >&2; exit 1 ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "k3d"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("TMPDIR", t.TempDir()) // keep kubeconfig cleanup away from real files
}

func TestClusterCleanupReportsFailedDelete(t *testing.T) {
	fakeK3d(t, "xp-test", "xp-e2e-leaky")
	c := &clusterCleanup{name: "xp-e2e-leaky"}
	_, err := c.finish(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "simulated delete failure") {
		t.Fatalf("finish = %v, want the k3d delete error", err)
	}
	if c.err == nil {
		t.Error("the error must be kept so Suite.Run can fail the run")
	}
}

func TestClusterCleanupSkipsPrefixMatches(t *testing.T) {
	// Only "xp-test" exists, so deleting "xp" must not call k3d cluster delete
	// (which the fake fails).
	fakeK3d(t, "xp-test")
	if err := (&clusterCleanup{name: "xp"}).run(); err != nil {
		t.Fatalf("run = %v, want nil", err)
	}
}

func TestSummarizeConditionsReportsMissing(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"generation": int64(2)},
		"status": map[string]any{"conditions": []any{
			map[string]any{"type": "Synced", "status": "False", "reason": "ReconcileError", "message": "boom"},
		}},
	}}
	got := summarizeConditions(u, []string{"Ready", "Synced"})
	want := "  Ready: not reported\n  Synced=False reason=ReconcileError generation=0/2 message=\"boom\""
	if got != want {
		t.Errorf("summarizeConditions =\n%s\nwant\n%s", got, want)
	}
}

func TestPollReportsTimeout(t *testing.T) {
	err := poll(context.Background(), 10*time.Millisecond, 50*time.Millisecond, func(context.Context) (bool, error) {
		return false, nil
	})
	if err == nil || !strings.Contains(err.Error(), "timed out after 50ms") {
		t.Errorf("poll = %v, want a timeout error", err)
	}
	boom := errors.New("boom")
	err = poll(context.Background(), 10*time.Millisecond, time.Second, func(context.Context) (bool, error) {
		return false, boom
	})
	if !errors.Is(err, boom) {
		t.Errorf("poll = %v, want %v", err, boom)
	}
}

func TestTrackDeduplicates(t *testing.T) {
	obj := func(kind, ns, name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion("example.org/v1")
		u.SetKind(kind)
		u.SetNamespace(ns)
		u.SetName(name)
		return u
	}
	list := track(nil, obj("Object", "a", "x"), obj("Object", "b", "x"))
	list = track(list, obj("Object", "a", "x"), obj("Other", "a", "x"))
	if len(list) != 3 {
		t.Errorf("track kept %d objects, want 3", len(list))
	}
}
