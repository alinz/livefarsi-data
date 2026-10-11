// Package source defines what a streaming site adapter must provide. Each
// adapter keeps its own working files in its own folder and syncs
// incrementally: it checks the site's listings for what changed and fetches
// only new or changed titles.
package source

import (
	"context"
	"sync"

	"farsi.ella.to/internal/catalog"
	"farsi.ella.to/internal/fetch"
)

// Source is one site, such as diycraftsguide.com or farsiplex.com.
type Source interface {
	// Name is the source's id; its files live in <data dir>/<Name>.
	Name() string
	// RPS is the most requests per second the source should receive.
	RPS() float64
	// SyncSeries brings the saved TV series up to date and returns all of
	// them. On error it still returns every series it has (from earlier runs
	// or this one), so one failure doesn't empty the catalog.
	SyncSeries(ctx context.Context, env *Env) ([]*catalog.Title, error)
	// SyncMovies does the same for movies.
	SyncMovies(ctx context.Context, env *Env) ([]*catalog.Title, error)
}

// Env is what a sync gets to work with.
type Env struct {
	Dir   string // the source's own folder, created before the sync starts
	Fetch *fetch.Fetcher
	Deep  bool // re-check every title, not only those the listings say changed
	Report
}

// Report receives a sync's progress.
type Report interface {
	// Phase starts a new step with total items to process.
	Phase(name string, total int)
	// Done marks one item of the current phase finished.
	Done()
	// Logf records a notable event, such as a new title or a failure.
	Logf(format string, args ...any)
}

// Parallel calls fn for each item (and its index) with at most n calls
// running at once. It stops starting new calls once ctx is done, and returns
// ctx's error then.
func Parallel[T any](ctx context.Context, n int, items []T, fn func(int, T)) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, n)
	for i, it := range items {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		}
		wg.Go(func() {
			defer func() { <-sem }()
			fn(i, it)
		})
	}
	wg.Wait()
	return ctx.Err()
}
