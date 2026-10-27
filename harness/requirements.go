package harness

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Environment variables that gate tests needing credentials or money.
const (
	// EnvPaid must be "true" for anything marked Paid to run. Without it a
	// plain `go test ./...` can never create billable resources.
	EnvPaid = "XP_E2E_PAID"
	// EnvStrict turns missing credentials into failures instead of skips.
	// Set it in CI jobs that are supposed to have credentials, so a broken
	// secret mapping can't silently skip the whole suite. It does not affect
	// EnvPaid: a run that has not opted in to paid tests always skips them.
	EnvStrict = "XP_E2E_STRICT"
)

// Requirements gate a test, or a whole suite, on its environment.
type Requirements struct {
	// Env lists environment variables (typically credentials) that must be
	// set and non-empty.
	Env []string
	// Paid marks tests that create billable external resources. They only run
	// when EnvPaid is "true".
	Paid bool
}

// verdict is what to do about a set of requirements.
type verdict int

const (
	run verdict = iota
	skip
	fail
)

// check decides whether requirements are met. lookup is os.LookupEnv in
// production. The returned reason never contains environment variable values.
func (r Requirements) check(lookup func(string) (string, bool)) (verdict, string) {
	if r.Paid && !envTrue(lookup, EnvPaid) {
		return skip, fmt.Sprintf("creates billable external resources; set %s=true to opt in", EnvPaid)
	}
	missing := missingEnv(lookup, r.Env)
	if len(missing) == 0 {
		return run, ""
	}
	reason := fmt.Sprintf("environment variables not set: %s", strings.Join(missing, ", "))
	if envTrue(lookup, EnvStrict) {
		return fail, reason + fmt.Sprintf(" (%s=true)", EnvStrict)
	}
	return skip, reason + fmt.Sprintf(" (set %s=true to fail instead of skipping)", EnvStrict)
}

// merge returns the union of r and o.
func (r Requirements) merge(o Requirements) Requirements {
	seen := map[string]bool{}
	var env []string
	for _, e := range append(append([]string{}, r.Env...), o.Env...) {
		if !seen[e] {
			seen[e] = true
			env = append(env, e)
		}
	}
	return Requirements{Env: env, Paid: r.Paid || o.Paid}
}

// Require skips the test (or, with EnvStrict, fails it) unless req is met,
// and returns the values of req.Env for use in checks against the external
// system. Call it at the top of a test, before Suite.Test:
//
//	func TestBucket(t *testing.T) {
//		creds := harness.Require(t, harness.Requirements{Env: []string{"ACME_TOKEN"}, Paid: true})
//		suite.Test(t, harness.ResourceTest{...})
//	}
//
// The returned values are secrets: never log them or put them in errors.
func Require(t testing.TB, req Requirements) map[string]string {
	t.Helper()
	switch v, reason := req.check(os.LookupEnv); v {
	case skip:
		t.Skip("skipping: " + reason)
	case fail:
		t.Fatal(reason)
	}
	vals := make(map[string]string, len(req.Env))
	for _, e := range req.Env {
		vals[e] = os.Getenv(e)
	}
	return vals
}

func missingEnv(lookup func(string) (string, bool), names []string) []string {
	var missing []string
	for _, n := range names {
		if v, ok := lookup(n); !ok || v == "" {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	return missing
}

func envTrue(lookup func(string) (string, bool), name string) bool {
	v, _ := lookup(name)
	b, err := strconv.ParseBool(v)
	return err == nil && b
}
