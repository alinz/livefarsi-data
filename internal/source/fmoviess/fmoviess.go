// Package fmoviess syncs the movies and TV series of ww.fmoviess.org.
//
// The site is large (hundreds of listing pages of 40 titles each), so a full
// sync only reads the listings, for each title's name and poster. A title's
// own page (description, seasons, players) is read when someone opens it, or
// syncs it, through SyncTitle.
//
// Listings are newest first. The first sync reads every page, saving a
// cursor after each one so an interrupted sync resumes where it stopped.
// Later syncs read from the top and stop at the first page with nothing new
// (deep syncs read every page again).
//
// Stored items: (movies, <slug>), (series, <slug>) for each season page, and
// (state, cursor-movies), (state, cursor-series) while a sync is underway.
package fmoviess

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"farsi.ella.to/internal/catalog"
	"farsi.ella.to/internal/fetch"
	"farsi.ella.to/internal/source"
)

// baseURL is a variable so tests can point it at a fake site.
var baseURL = "https://ww.fmoviess.org"

// perPage spaces out the sort keys of one listing page; pages hold ~40 cards.
const perPage = 1000

type Source struct {
	// mu serializes read-modify-write of items, so a title sync saving a
	// title's details and a listing sync updating its card don't overwrite
	// each other.
	mu sync.Mutex
}

func New() *Source { return &Source{} }

func (*Source) Name() string    { return "fmoviess" }
func (*Source) Section() string { return source.Foreign }
func (*Source) RPS() float64    { return 4 }

// listingURL is page n of the movie or TV listing. Page 2 on is linked with
// a double slash ("/movies//2/"); the site serves it as-is.
func listingURL(kind string, n int) string {
	dir := "movies"
	if kind == source.Series {
		dir = "tv-series"
	}
	if n <= 1 {
		return fmt.Sprintf("%s/%s/", baseURL, dir)
	}
	return fmt.Sprintf("%s/%s//%d/", baseURL, dir, n)
}

// cursor is where an unfinished listing sync stands.
type cursor struct {
	Next    int       `json:"next"`    // the next page to read
	Last    int       `json:"last"`    // the last page, as of the latest page read
	Base    int64     `json:"base"`    // sort key base for titles new in this run
	Full    bool      `json:"full"`    // read every page (first sync, or deep)
	Started time.Time `json:"started"` // when this run began
}

func (s *Source) SyncSeries(ctx context.Context, env *source.Env) ([]*catalog.Title, error) {
	return s.sync(ctx, env, source.Series)
}

func (s *Source) SyncMovies(ctx context.Context, env *source.Env) ([]*catalog.Title, error) {
	return s.sync(ctx, env, source.Movies)
}

// sync reads the listing of one kind, then returns the catalog built from
// every saved item (also on error, so a failure doesn't empty the catalog).
func (s *Source) sync(ctx context.Context, env *source.Env, kind string) ([]*catalog.Title, error) {
	err := s.syncListing(ctx, env, kind)
	titles, lerr := s.titles(ctx, env, kind)
	return titles, errors.Join(err, lerr)
}

func (s *Source) syncListing(ctx context.Context, env *source.Env, kind string) error {
	items, err := s.items(ctx, env, kind)
	if err != nil {
		return err
	}
	minSort := int64(0)
	for _, it := range items {
		minSort = min(minSort, it.Sort)
	}

	curKey := "cursor-" + kind
	var cur *cursor
	if ok, err := env.Store.Load(ctx, "state", curKey, &cur); err != nil || !ok {
		cur = nil
	}
	if cur != nil {
		env.Logf("%s: resuming at page %d of %d (started %s)", kind, cur.Next, cur.Last, cur.Started.Format(time.DateTime))
		cur.Full = cur.Full || env.Deep
	} else {
		cur = &cursor{Next: 1, Full: len(items) == 0 || env.Deep, Started: time.Now().UTC()}
	}

	env.Phase(kind, max(cur.Last, 1))
	for range cur.Next - 1 {
		env.Done()
	}
	for cur.Last == 0 || cur.Next <= cur.Last {
		n := cur.Next
		body, err := env.Fetch.Get(ctx, listingURL(kind, n))
		if errors.Is(err, fetch.ErrNotFound) && n > 1 {
			break // past the end: the listing shrank since the last page count
		}
		if err != nil {
			return fmt.Errorf("page %d: %w", n, err)
		}
		cards, last := parseListing(body)
		if len(cards) == 0 {
			if n == 1 {
				return fmt.Errorf("page 1 lists no titles; has the site's layout changed?")
			}
			break
		}
		if cur.Last == 0 {
			// Titles new in this run sort before everything already saved.
			cur.Base = minSort - int64(max(last, 1)+1)*perPage
		}
		if last = max(last, n); last > cur.Last {
			cur.Last = last
			env.Phase(kind, cur.Last)
			for range n - 1 {
				env.Done()
			}
		}

		added, err := s.saveCards(ctx, env, kind, cards, cur.Base+int64(n)*perPage)
		if err != nil {
			return err
		}
		if added > 0 {
			env.Logf("+ %s page %d: %d new", kind, n, added)
		}
		cur.Next = n + 1
		if err := env.Store.Save(ctx, "state", curKey, cur); err != nil {
			return err
		}
		env.Done()
		if added == 0 && !cur.Full {
			env.Logf("%s: page %d has nothing new; caught up", kind, n)
			break
		}
	}
	return env.Store.Delete(ctx, "state", curKey)
}

// saveCards stores one listing page's cards. Known titles get their card
// fields refreshed and keep their details and place; new ones are sorted by
// base and their position on the page. It returns how many were new.
func (s *Source) saveCards(ctx context.Context, env *source.Env, kind string, cards []card, base int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	added := 0
	for i, c := range cards {
		var it *Item
		if ok, err := env.Store.Load(ctx, kind, c.slug, &it); err != nil {
			return added, err
		} else if !ok {
			it = &Item{Slug: c.slug, Sort: base + int64(i)}
			added++
		}
		before, _ := json.Marshal(it)
		it.URL, it.Title, it.Badge = c.url, c.title, c.badge
		if c.poster != "" {
			it.Poster = c.poster
		}
		if kind == source.Series {
			it.Show, it.Season = splitSeason(c.title)
		}
		if after, _ := json.Marshal(it); string(after) == string(before) {
			continue
		}
		if err := env.Store.Save(ctx, kind, c.slug, it); err != nil {
			return added, err
		}
	}
	return added, nil
}

// items loads every saved item of a kind.
func (s *Source) items(ctx context.Context, env *source.Env, kind string) ([]*Item, error) {
	var out []*Item
	err := env.Store.Each(ctx, kind, func(_ string, data []byte) error {
		it := &Item{}
		if err := json.Unmarshal(data, it); err != nil {
			return err
		}
		out = append(out, it)
		return nil
	})
	return out, err
}

// titles builds the catalog of one kind from the saved items, newest first.
func (s *Source) titles(ctx context.Context, env *source.Env, kind string) ([]*catalog.Title, error) {
	items, err := s.items(ctx, env, kind)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(items, func(a, b *Item) int { return cmp.Compare(a.Sort, b.Sort) })
	if kind == source.Movies {
		out := make([]*catalog.Title, len(items))
		for i, it := range items {
			out[i] = s.movieTitle(it)
		}
		return out, nil
	}

	// Group seasons into shows, placed where their newest season is. Names
	// alone can't tell apart different shows with the same name (The Office
	// US, UK, ...), so a name whose seasons repeat a number isn't grouped:
	// its seasons stay separate titles, as the site lists them.
	var order []string
	shows := map[string][]*Item{}
	for _, it := range items {
		k := showKey(it)
		if shows[k] == nil {
			order = append(order, k)
		}
		shows[k] = append(shows[k], it)
	}
	var out []*catalog.Title
	for _, k := range order {
		group := shows[k]
		if !distinctSeasons(group) {
			for _, it := range group {
				out = append(out, s.seasonTitle(it))
			}
			continue
		}
		out = append(out, s.showTitle(group))
	}
	return out, nil
}

func distinctSeasons(items []*Item) bool {
	seen := map[int]bool{}
	for _, it := range items {
		if seen[it.Season] {
			return false
		}
		seen[it.Season] = true
	}
	return true
}

// seasonTitle makes one series of a single season, named as the site does
// ("The Office - Season 1").
func (s *Source) seasonTitle(it *Item) *catalog.Title {
	t := s.showTitle([]*Item{it})
	t.ID, t.Title = catalog.Slug(it.Title), it.Title
	return t
}

func showKey(it *Item) string {
	return strings.ToLower(cmpOr(it.Show, it.Title))
}

func (s *Source) movieTitle(it *Item) *catalog.Title {
	return &catalog.Title{
		ID:          catalog.Slug(it.Title),
		Title:       it.Title,
		Poster:      it.Poster,
		Description: it.Description,
		Links:       []catalog.Link{{Source: s.Name(), URL: it.URL}},
		Players:     s.players(it),
		Partial:     it.SyncedAt.IsZero(),
	}
}

// showTitle makes one series of a show's season items; the first (newest)
// item gives the poster.
func (s *Source) showTitle(seasons []*Item) *catalog.Title {
	first := seasons[0]
	t := &catalog.Title{
		ID:     catalog.Slug(cmpOr(first.Show, first.Title)),
		Title:  cmpOr(first.Show, first.Title),
		Poster: first.Poster,
	}
	sorted := slices.Clone(seasons)
	slices.SortStableFunc(sorted, func(a, b *Item) int { return cmp.Compare(a.Season, b.Season) })
	for _, it := range sorted {
		t.Links = append(t.Links, catalog.Link{Source: s.Name(), URL: it.URL})
		t.Description = cmpOr(t.Description, it.Description)
		if it.SyncedAt.IsZero() {
			t.Partial = true
			continue
		}
		se := &catalog.Season{Number: it.Season, Name: fmt.Sprintf("Season %d", it.Season)}
		for _, ep := range it.Episodes {
			se.Episodes = append(se.Episodes, &catalog.Episode{
				Number:  ep.Number,
				Title:   ep.Title,
				Players: s.players(it),
			})
		}
		t.Seasons = append(t.Seasons, se)
	}
	return t
}

// players links to the title's page on the site: its player can't be
// embedded (see Detail). A title not synced yet has none.
func (s *Source) players(it *Item) []*catalog.Player {
	if it.SyncedAt.IsZero() {
		return nil
	}
	return []*catalog.Player{{Source: s.Name(), Type: catalog.PlayerLink, URL: it.URL}}
}

// ---- one title ----

// SyncTitle reads a title's own page (each season's page, for a series) and
// saves its details.
func (s *Source) SyncTitle(ctx context.Context, env *source.Env, kind string, t *catalog.Title) (*catalog.Title, error) {
	var urls []string
	for _, l := range t.Links {
		if l.Source == s.Name() {
			urls = append(urls, l.URL)
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("%s has no %s page", t.Title, s.Name())
	}

	items := make([]*Item, len(urls))
	errs := make([]error, len(urls))
	source.Parallel(ctx, 2, urls, func(i int, u string) {
		items[i], errs[i] = s.syncPage(ctx, env, kind, u)
	})
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if kind == source.Movies {
		return s.movieTitle(items[0]), nil
	}
	slices.SortStableFunc(items, func(a, b *Item) int { return cmp.Compare(a.Sort, b.Sort) })
	nt := s.showTitle(items)
	// Keep the title's identity: it may be one season listed on its own.
	if t.ID != "" {
		nt.ID, nt.Title = t.ID, t.Title
	}
	return nt, nil
}

// syncPage reads one title page and saves what it found into the item.
func (s *Source) syncPage(ctx context.Context, env *source.Env, kind, u string) (*Item, error) {
	page, err := env.Fetch.Get(ctx, u)
	if err != nil {
		return nil, err
	}
	title, poster, d := parsePage(page)
	slug := path.Base(strings.TrimSuffix(u, "/"))

	s.mu.Lock()
	defer s.mu.Unlock()
	var it *Item
	if ok, err := env.Store.Load(ctx, kind, slug, &it); err != nil {
		return nil, err
	} else if !ok {
		it = &Item{Slug: slug, URL: u, Title: title}
		if kind == source.Series {
			it.Show, it.Season = splitSeason(title)
		}
	}
	it.Detail = d
	it.SyncedAt = time.Now().UTC()
	it.Poster = cmpOr(it.Poster, poster)
	if err := env.Store.Save(ctx, kind, slug, it); err != nil {
		return nil, err
	}
	env.Logf("~ %s %s: %d episode(s), %d server(s)", kind, slug, len(d.Episodes), len(d.Servers))
	return it, nil
}
