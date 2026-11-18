// Package diycraftsguide syncs the TV series and movies of LiveFarsi, which
// is served from www.diycraftsguide.com.
//
// Series: the catalog pages list every show with its episode count, so a show
// whose count matches its saved file is skipped without a request. A changed
// show re-reads its page, keeps the players it already has, and fetches only
// new episode pages. Movies: the catalog lists every movie; only movies not
// yet saved are fetched. Deep mode re-reads every show and movie page.
//
// Stored items: (series, <slug>) and (movies, <slug>).
package diycraftsguide

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"sync/atomic"

	"farsi.ella.to/internal/catalog"
	"farsi.ella.to/internal/fetch"
	"farsi.ella.to/internal/source"
)

const baseURL = "https://www.diycraftsguide.com"

type Source struct{}

func New() *Source { return &Source{} }

func (*Source) Name() string    { return "diycraftsguide" }
func (*Source) Section() string { return source.Farsi }
func (*Source) RPS() float64    { return 8 }

// listCatalog reads every page of a catalog and returns its titles in order.
// first is the first page; page(n) is the URL of a later one, where n is the
// number in the site's pagination links: start, start+step, ... up to the
// number in the "last page" link.
func listCatalog(ctx context.Context, f *fetch.Fetcher, first string, start, step int, page func(n int) string) ([]card, error) {
	body, err := f.Get(ctx, first)
	if err != nil {
		return nil, err
	}
	cards, last := parseCatalogPage(body)

	// Fetch the other pages concurrently (the fetcher still rate-limits),
	// then append them in page order.
	var nums []int
	for n := start; n <= last; n += step {
		nums = append(nums, n)
	}
	pages := make([][]card, len(nums))
	errs := make([]error, len(nums))
	var wg sync.WaitGroup
	for i, n := range nums {
		wg.Go(func() {
			body, err := f.Get(ctx, page(n))
			pages[i], _ = parseCatalogPage(body)
			errs[i] = err
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	for _, p := range pages {
		cards = append(cards, p...)
	}

	seen := map[string]bool{}
	unique := cards[:0]
	for _, c := range cards {
		if !seen[c.slug] {
			seen[c.slug] = true
			unique = append(unique, c)
		}
	}
	return unique, nil
}

// ---- series ----

func (s *Source) SyncSeries(ctx context.Context, env *source.Env) ([]*catalog.Title, error) {
	cards, err := listCatalog(ctx, env.Fetch, baseURL+"/serial.html", 24, 24, func(n int) string {
		return fmt.Sprintf("%s/tv-series/%d.html", baseURL, n)
	})
	if err != nil {
		return nil, err
	}
	env.Phase("series", len(cards))

	shows := make([]*Show, len(cards))
	var failed atomic.Int32
	err = source.Parallel(ctx, 4, cards, func(i int, c card) {
		defer env.Done()
		show, err := s.syncShow(ctx, env, c)
		if err != nil && ctx.Err() == nil {
			failed.Add(1)
			env.Logf("%s: %v (will retry on next sync)", c.slug, err)
		}
		shows[i] = show
	})

	var titles []*catalog.Title
	for _, sh := range shows {
		if sh != nil {
			titles = append(titles, s.showTitle(sh))
		}
	}
	if err != nil {
		return titles, err
	}
	if n := failed.Load(); n > 0 {
		return titles, fmt.Errorf("%d series failed", n)
	}
	return titles, nil
}

// syncShow brings one show's file up to date and returns the show. On error
// it returns the previously saved show, if any.
func (s *Source) syncShow(ctx context.Context, env *source.Env, c card) (*Show, error) {
	var old *Show
	if ok, err := env.Store.Load(ctx, source.Series, c.slug, &old); err != nil || !ok {
		old = nil
	}
	if old != nil && !env.Deep && old.EpisodeCount == c.label {
		return old, nil
	}

	show := &Show{Slug: c.slug, Title: c.title, URL: c.url, Poster: c.poster, EpisodeCount: c.label}
	known := map[string]*Episode{}
	if old != nil {
		for _, ep := range old.episodes() {
			known[ep.URL] = ep
		}
	}
	added, err := scrapeShow(ctx, env.Fetch, show, known)
	if err != nil {
		return old, err
	}

	b, err := catalog.Marshal(show)
	if err != nil {
		return old, err
	}
	if old != nil {
		if prev, err := catalog.Marshal(old); err == nil && bytes.Equal(prev, b) {
			return old, nil
		}
	}
	if err := env.Store.Save(ctx, source.Series, c.slug, show); err != nil {
		return old, err
	}
	switch {
	case old == nil:
		env.Logf("+ series %s: %d episodes", c.slug, added)
	case added > 0:
		env.Logf("~ series %s: %d new episode(s)", c.slug, added)
	default:
		env.Logf("~ series %s: updated", c.slug)
	}
	return show, nil
}

// scrapeShow reads the show page, then the episode pages for their players.
// Episodes found in known (from the previous sync) keep their saved player,
// so only new episodes, or ones that had no player last time, are fetched.
func scrapeShow(ctx context.Context, f *fetch.Fetcher, s *Show, known map[string]*Episode) (added int, err error) {
	page, err := f.Get(ctx, s.URL)
	if err != nil {
		return 0, err
	}
	parseShowPage(s, page)

	var todo []*Episode
	for _, ep := range s.episodes() {
		prev, ok := known[ep.URL]
		if !ok {
			added++
		} else if ep.Embed == "" {
			ep.Video = prev.Video
		}
		if ep.Embed == "" {
			todo = append(todo, ep)
		}
	}

	var failed atomic.Int32
	var first atomic.Value
	err = source.Parallel(ctx, 3, todo, func(_ int, ep *Episode) {
		page, err := f.Get(ctx, ep.URL)
		switch {
		case err == nil:
			ep.Video = parsePlayer(page)
		case errors.Is(err, fetch.ErrNotFound):
			// Gone from the site; leave it without a player.
		default:
			failed.Add(1)
			first.CompareAndSwap(nil, err)
		}
	})
	if err != nil {
		return 0, err
	}
	if n := failed.Load(); n > 0 {
		return 0, fmt.Errorf("%d episode pages failed, e.g. %v", n, first.Load())
	}
	return added, nil
}

func (s *Source) showTitle(sh *Show) *catalog.Title {
	t := &catalog.Title{
		ID:      sh.Slug,
		Title:   sh.Title,
		TitleFa: persianTitle(sh.Description),
		Poster:  sh.Poster,
		Links:   []catalog.Link{{Source: s.Name(), URL: sh.URL}},
	}
	for _, se := range sh.Seasons {
		cse := &catalog.Season{Number: catalog.SeasonNumber(se.Name), Name: se.Name}
		for _, ep := range se.Episodes {
			cse.Episodes = append(cse.Episodes, &catalog.Episode{
				Number:    catalog.EpisodeNumber(ep.Title),
				Title:     ep.Title,
				Thumbnail: ep.Thumbnail,
				Players:   s.players(ep.Video),
			})
		}
		t.Seasons = append(t.Seasons, cse)
	}
	return t
}

func (s *Source) players(v Video) []*catalog.Player {
	if v.Embed == "" {
		return nil
	}
	return []*catalog.Player{{Source: s.Name(), Type: v.Player, URL: v.Embed, Mime: v.MimeType}}
}

// ---- movies ----

func (s *Source) SyncMovies(ctx context.Context, env *source.Env) ([]*catalog.Title, error) {
	cards, err := listCatalog(ctx, env.Fetch, baseURL+"/movies.html", 2, 1, func(n int) string {
		return fmt.Sprintf("%s/movies/%d.html", baseURL, n)
	})
	if err != nil {
		return nil, err
	}
	env.Phase("movies", len(cards))

	movies := make([]*Movie, len(cards))
	var failed atomic.Int32
	err = source.Parallel(ctx, 6, cards, func(i int, c card) {
		defer env.Done()
		var old *Movie
		if ok, err := env.Store.Load(ctx, source.Movies, c.slug, &old); err != nil || !ok {
			old = nil
		}
		movies[i] = old
		if old != nil && !env.Deep {
			return
		}

		m := &Movie{Slug: c.slug, Title: c.title, URL: c.url, Poster: c.poster}
		page, err := env.Fetch.Get(ctx, c.url)
		if err != nil {
			if ctx.Err() == nil {
				failed.Add(1)
				env.Logf("%s: %v (will retry on next sync)", c.slug, err)
			}
			return
		}
		parseMoviePage(m, page)
		if old != nil && *old == *m {
			return
		}
		if err := env.Store.Save(ctx, source.Movies, c.slug, m); err != nil {
			failed.Add(1)
			env.Logf("%s: %v", c.slug, err)
			return
		}
		movies[i] = m
		if old == nil {
			env.Logf("+ movie %s", c.slug)
		} else {
			env.Logf("~ movie %s: updated", c.slug)
		}
	})

	var titles []*catalog.Title
	for _, m := range movies {
		if m != nil {
			titles = append(titles, s.movieTitle(m))
		}
	}
	if err != nil {
		return titles, err
	}
	if n := failed.Load(); n > 0 {
		return titles, fmt.Errorf("%d movies failed", n)
	}
	return titles, nil
}

func (s *Source) movieTitle(m *Movie) *catalog.Title {
	t := &catalog.Title{
		ID:      m.Slug,
		Title:   m.Title,
		TitleFa: persianTitle(m.Description),
		Poster:  m.Poster,
		Links:   []catalog.Link{{Source: s.Name(), URL: m.URL}},
		Players: s.players(m.Video),
	}
	return t
}

// ---- one title ----

// SyncTitle re-reads one show page (fetching only episodes it doesn't have a
// player for yet, unless env.Deep) or one movie page.
func (s *Source) SyncTitle(ctx context.Context, env *source.Env, kind string, t *catalog.Title) (*catalog.Title, error) {
	u := source.LinkOf(t, s.Name())
	if u == "" {
		return nil, fmt.Errorf("%s has no %s page", t.Title, s.Name())
	}
	slug := strings.TrimSuffix(path.Base(u), ".html")

	if kind == source.Series {
		var old *Show
		if ok, _ := env.Store.Load(ctx, source.Series, slug, &old); !ok {
			old = nil
		}
		c := card{slug: slug, title: t.Title, url: u, poster: t.Poster}
		if old != nil {
			c.title, c.poster, c.label = old.Title, old.Poster, old.EpisodeCount
		}
		// An empty label never matches the saved count, so the page is re-read.
		saved := c.label
		c.label = ""
		show, err := s.syncShow(ctx, env, c)
		if err != nil {
			return nil, err
		}
		if show.EpisodeCount == "" && saved != "" {
			// Keep the catalog's count so the next full sync can skip it.
			show.EpisodeCount = saved
			if err := env.Store.Save(ctx, source.Series, slug, show); err != nil {
				return nil, err
			}
		}
		return s.showTitle(show), nil
	}

	m := &Movie{Slug: slug, Title: t.Title, URL: u, Poster: t.Poster}
	page, err := env.Fetch.Get(ctx, u)
	if err != nil {
		return nil, err
	}
	parseMoviePage(m, page)
	if err := env.Store.Save(ctx, source.Movies, slug, m); err != nil {
		return nil, err
	}
	env.Logf("~ movie %s: synced", slug)
	return s.movieTitle(m), nil
}
