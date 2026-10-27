package harness

import (
	"context"
	"log"
	"sync"
	"time"

	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// clusterCleanup deletes a suite's generated cluster exactly once. Interrupts
// and the approaching -timeout don't delete it directly: watchInterrupts
// stops the tests so the Finish steps run in order, deleting managed
// resources (and so external resources) before this deletes the cluster.
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
