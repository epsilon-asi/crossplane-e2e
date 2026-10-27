package harness

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackageEnvVar(t *testing.T) {
	if got, want := PackageEnvVar("provider-kubernetes"), "XP_E2E_PACKAGE_PROVIDER_KUBERNETES"; got != want {
		t.Errorf("PackageEnvVar = %q, want %q", got, want)
	}
}

func TestIsLocalPackage(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "provider.xpkg")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		file: true,
		dir:  false,
		"xpkg.crossplane.io/crossplane-contrib/provider-kubernetes:v1.4.1": false,
	}
	for pkg, want := range cases {
		if got := isLocalPackage(pkg); got != want {
			t.Errorf("isLocalPackage(%q) = %t, want %t", pkg, got, want)
		}
	}
}

func TestLooksLikePath(t *testing.T) {
	for pkg, want := range map[string]bool{
		"/abs/provider.xpkg":                       true,
		"_output/x/provider.xpkg":                  true,
		"./provider":                               true,
		"../provider":                              true,
		"xpkg.crossplane.io/a/b:v1":                false,
		"registry.localhost:5000/provider-foo:dev": false,
	} {
		if got := looksLikePath(pkg); got != want {
			t.Errorf("looksLikePath(%q) = %t, want %t", pkg, got, want)
		}
	}
}

func TestValidRunID(t *testing.T) {
	for id, want := range map[string]bool{
		"abc123":                true,
		"18837465522-1":         true,
		"a":                     true,
		"":                      false,
		"-abc":                  false,
		"abc-":                  false,
		"ABC":                   false,
		"a_b":                   false,
		"abcdefghij0123456789":  true,
		"abcdefghij0123456789x": false,
	} {
		if got := validRunID.MatchString(id); got != want {
			t.Errorf("validRunID(%q) = %t, want %t", id, got, want)
		}
	}
}
