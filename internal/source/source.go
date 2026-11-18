// Package source defines what a streaming site adapter must provide. Each
// adapter keeps its own working data in the store and syncs incrementally:
// it checks the site's listings for what changed and fetches only new or
// changed titles.
package source

import (
	"context"
	"sync"

	"farsi.ella.to/internal/catalog"
	"farsi.ella.to/internal/fetch"
)

// Sections group sources into the site's top-level parts. Each section is
// synced, merged and served on its own.
const (
	Farsi   = "farsi"   // Iranian series and movies
	Foreign = "foreign" // everything else
)

// Kinds of titles, as used in the store and the API.
const (
	Series = "series"
	Movies = "movies"
)

// Source is one site, such as diycraftsguide.com or farsiplex.com.
type Source interface {
	// Name is the source's id; its items are stored under it.
	Name() string
	// Section is the section (Farsi or Foreign) the source belongs to.
	Section() string
	// RPS is the most requests per second the source should receive.
	RPS() float64
	// SyncSeries brings the saved TV series up to date and returns all of
	// them. On error it still returns every series it has (from earlier runs
	// or this one), so one failure doesn't empty the catalog.
	SyncSeries(ctx context.Context, env *Env) ([]*catalog.Title, error)
	// SyncMovies does the same for movies.
	SyncMovies(ctx context.Context, env *Env) ([]*catalog.Title, error)
	// SyncTitle brings one title up to date: t is this source's own version
	// of it (as SyncSeries or SyncMovies returned it), kind is Series or
	// Movies. It returns the updated title.
	SyncTitle(ctx context.Context, env *Env, kind string, t *catalog.Title) (*catalog.Title, error)
}

// Store is a source's own persistent data: one JSON document per (kind,
// slug). Sources use the kinds Series and Movies for titles, and "state" for
// anything else they need to remember, such as where a sync left off.
type Store interface {
	// Load reads a document into v; it reports false if there is none.
	Load(ctx context.Context, kind, slug string, v any) (bool, error)
	Save(ctx context.Context, kind, slug string, v any) error
	Delete(ctx context.Context, kind, slug string) error
	// Each calls fn with every document of a kind.
	Each(ctx context.Context, kind string, fn func(slug string, data []byte) error) error
}

// Env is what a sync gets to work with.
type Env struct {
	Store Store
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

// LinkOf returns the URL of t's page on the named source, or "".
func LinkOf(t *catalog.Title, source string) string {
	for _, l := range t.Links {
		if l.Source == source {
			return l.URL
		}
	}
	return ""
}
