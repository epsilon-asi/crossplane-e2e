package harness

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"
)

// Environment variables for sweepers.
const (
	// EnvSweep runs the suite's sweepers on their own instead of its tests:
	// no cluster is created. "all" sweeps every e2e- resource; a run ID
	// sweeps only that run's.
	EnvSweep = "XP_E2E_SWEEP"
	// EnvSweepRun limits a sweep to the named sweepers (comma separated)
	// and their dependencies.
	EnvSweepRun = "XP_E2E_SWEEP_RUN"
)

// NamePrefix starts the name of every external resource a suite creates, so
// that sweepers can find them: name them e.g. e2e-${RUN_ID}-bucket.
const NamePrefix = "e2e-"

// Sweeper finds and deletes external resources left behind by test runs, in
// the spirit of Terraform's test sweepers. Managed resources are normally
// deleted through Crossplane, but a run that is killed, times out or loses
// its cluster leaves external resources that nothing in Kubernetes knows
// about any more. Sweepers talk to the external API directly.
//
// Sweepers run at the end of every suite (scoped to that run, after its
// managed resources have been deleted) and can be run on their own with
// EnvSweep.
type Sweeper struct {
	// Name identifies the sweeper, e.g. "buckets".
	Name string
	// Requires lists the environment variables (credentials) the sweeper
	// needs. When unset the sweeper is skipped at the end of a suite and
	// fails a standalone sweep.
	Requires []string
	// Dependencies name sweepers that must run first, e.g. sweep instances
	// before the networks they use.
	Dependencies []string
	// Sweep deletes every external resource whose name scope.Matches. It
	// should be idempotent and tolerate resources that are already gone.
	Sweep func(ctx context.Context, scope SweepScope) error
}

// SweepScope says which external resources a sweeper may delete.
type SweepScope struct {
	// RunID restricts the sweep to one run's resources. Empty means every
	// resource named with NamePrefix, from any run, including runs that may
	// still be in progress elsewhere.
	RunID string
}

func (s SweepScope) String() string {
	if s.RunID == "" {
		return "all " + NamePrefix + "* resources"
	}
	return "run " + s.RunID
}

// Matches reports whether an external resource name is in scope: it must
// start with NamePrefix and, for a run-scoped sweep, contain the run ID as a
// whole token (delimited by anything but letters and digits).
func (s SweepScope) Matches(name string) bool {
	if !strings.HasPrefix(strings.ToLower(name), NamePrefix) {
		return false
	}
	if s.RunID == "" {
		return true
	}
	re := regexp.MustCompile(`(?i)(^|[^a-z0-9])` + regexp.QuoteMeta(s.RunID) + `([^a-z0-9]|$)`)
	return re.MatchString(name)
}

var runIDPattern = regexp.MustCompile(`^[0-9a-f]{6}$`)

// parseSweepScope parses the value of EnvSweep.
func parseSweepScope(v string) (SweepScope, error) {
	switch {
	case v == "all":
		return SweepScope{}, nil
	case runIDPattern.MatchString(v):
		return SweepScope{RunID: v}, nil
	}
	return SweepScope{}, fmt.Errorf(`%s=%q: want "all" or a 6 hex digit run ID`, EnvSweep, v)
}

// selectSweepers returns the sweepers named in filter (comma separated; all
// when empty) plus their dependencies, ordered so dependencies run first and
// otherwise in declaration order.
func selectSweepers(all []Sweeper, filter string) ([]Sweeper, error) {
	byName := map[string]Sweeper{}
	for _, s := range all {
		if s.Name == "" || s.Sweep == nil {
			return nil, errors.New("sweeper: Name and Sweep are required")
		}
		if _, dup := byName[s.Name]; dup {
			return nil, fmt.Errorf("sweeper %q declared twice", s.Name)
		}
		byName[s.Name] = s
	}

	var roots []string
	for _, n := range strings.Split(filter, ",") {
		if n = strings.TrimSpace(n); n != "" {
			roots = append(roots, n)
		}
	}
	if len(roots) == 0 {
		for _, s := range all {
			roots = append(roots, s.Name)
		}
	}

	var out []Sweeper
	state := map[string]int{} // 1 visiting, 2 done
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		s, ok := byName[name]
		if !ok {
			return fmt.Errorf("unknown sweeper %q", name)
		}
		switch state[name] {
		case 1:
			return fmt.Errorf("sweeper dependency cycle: %s", strings.Join(append(path, name), " -> "))
		case 2:
			return nil
		}
		state[name] = 1
		for _, d := range s.Dependencies {
			if err := visit(d, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		out = append(out, s)
		return nil
	}
	for _, r := range roots {
		if err := visit(r, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// runSweepers runs sweepers in order. Sweepers whose requirements are missing
// are skipped, or are an error when strict. A failing sweeper does not stop
// the others.
func runSweepers(ctx context.Context, sweepers []Sweeper, scope SweepScope, lookup func(string) (string, bool), strict bool) error {
	var errs []error
	for _, s := range sweepers {
		if missing := missingEnv(lookup, s.Requires); len(missing) > 0 {
			msg := fmt.Sprintf("sweeper %s: environment variables not set: %s", s.Name, strings.Join(missing, ", "))
			if strict {
				errs = append(errs, errors.New(msg))
			} else {
				log.Printf("%s; skipped", msg)
			}
			continue
		}
		start := time.Now()
		log.Printf("sweeper %s: sweeping %s", s.Name, scope)
		if err := s.Sweep(ctx, scope); err != nil {
			errs = append(errs, fmt.Errorf("sweeper %s: %w", s.Name, err))
			continue
		}
		log.Printf("sweeper %s: done in %s", s.Name, time.Since(start).Round(time.Second))
	}
	return errors.Join(errs...)
}

// sweepOnly implements EnvSweep: run the selected sweepers without a cluster
// or tests and return the exit code.
func (s *Suite) sweepOnly(v string) int {
	scope, err := parseSweepScope(v)
	if err != nil {
		log.Print(err)
		return 1
	}
	sweepers, err := selectSweepers(s.cfg.Sweepers, os.Getenv(EnvSweepRun))
	if err != nil {
		log.Print(err)
		return 1
	}
	if len(sweepers) == 0 {
		log.Printf("%s: this suite has no sweepers", EnvSweep)
		return 0
	}
	ctx, cancel := context.WithTimeout(s.ctx, s.cleanupTimeout())
	defer cancel()
	if err := runSweepers(ctx, sweepers, scope, os.LookupEnv, true); err != nil {
		log.Printf("sweep failed: %v", err)
		return 1
	}
	return 0
}
