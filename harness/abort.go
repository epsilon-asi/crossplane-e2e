package harness

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// timeoutMargin is how long before the test binary's -timeout the cluster is
// deleted, so a run that hangs does not leak it.
const timeoutMargin = 30 * time.Second

// clusterCleanup deletes a suite's generated cluster exactly once, whichever
// comes first: the end of the run, an interrupt, or the approaching -timeout.
type clusterCleanup struct {
	name string
	once sync.Once
	err  error
}

func (c *clusterCleanup) run() error {
	c.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if c.err = destroyCluster(ctx, c.name); c.err != nil {
			log.Printf("ERROR: cluster %q may have leaked: %v\n  delete it with: k3d cluster delete %s", c.name, c.err, c.name)
		}
	})
	return c.err
}

// finish is registered as an env.Finish action. e2e-framework runs Finish
// actions even when env Setup fails or the suite panics, but only logs their
// errors at a verbosity that is off by default, so run reports them itself and
// Suite.Run turns them into a failing exit code.
func (c *clusterCleanup) finish(ctx context.Context, _ *envconf.Config) (context.Context, error) {
	return ctx, c.run()
}

// watch deletes the cluster if the run is interrupted (SIGINT, SIGTERM) or is
// about to hit the test binary's -timeout, neither of which runs Finish
// actions. On a signal it exits once the cluster is gone; on the timeout it
// leaves the run to fail and report where it was stuck. Call the returned
// function once the suite has finished.
func (c *clusterCleanup) watch() (stop func()) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	var deadline <-chan time.Time
	var timer *time.Timer
	if d := testTimeout(); d > 2*timeoutMargin {
		timer = time.NewTimer(d - timeoutMargin)
		deadline = timer.C
	}

	done := make(chan struct{})
	go func() {
		select {
		case s := <-sigs:
			signal.Stop(sigs) // a second interrupt kills the process outright
			log.Printf("received %s: deleting cluster %q before exiting (interrupt again to skip)", s, c.name)
			_ = c.run()
			os.Exit(1)
		case <-deadline:
			log.Printf("run is %s from its -timeout: deleting cluster %q now so the timeout does not leak it", timeoutMargin, c.name)
			_ = c.run()
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigs)
		if timer != nil {
			timer.Stop()
		}
		close(done)
	}
}

// testTimeout returns the value of go test's -timeout flag, or 0.
func testTimeout() time.Duration {
	f := flag.Lookup("test.timeout")
	if f == nil {
		return 0
	}
	g, ok := f.Value.(flag.Getter)
	if !ok {
		return 0
	}
	d, _ := g.Get().(time.Duration)
	return d
}
