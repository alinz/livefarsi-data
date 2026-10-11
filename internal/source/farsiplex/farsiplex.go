// Package farsiplex syncs the TV series and movies of farsiplex.com.
//
// The site is WordPress, and its sitemaps list every movie, show and episode
// with the date it last changed, so a sync reads the sitemaps (a few
// requests) and fetches only what is new or has a newer date. Players come
// from the site's player API, one per quality.
//
// Files: <dir>/series/<slug>.json and <dir>/movies/<slug>.json.
package farsiplex

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"

	"farsi.ella.to/internal/catalog"
	"farsi.ella.to/internal/fetch"
	"farsi.ella.to/internal/source"
)

const baseURL = "https://farsiplex.com"

// maxPlayers caps how many qualities are probed per title (the site has
// 480p, 720p and 1080p).
const maxPlayers = 5

type Source struct{}

func New() *Source { return &Source{} }

func (*Source) Name() string { return "farsiplex" }

// RPS is low because the player API isn't cached on the site's side.
func (*Source) RPS() float64 { return 5 }

// ---- sitemaps ----

type entry struct{ loc, lastmod string }

var (
	reSitemap = regexp.MustCompile(`<loc>([^<]*/wp-sitemap-posts-(\w+)-\d+\.xml)</loc>`)
	reURL     = regexp.MustCompile(`(?s)<url>\s*<loc>([^<]+)</loc>(?:\s*<lastmod>([^<]+)</lastmod>)?`)
)

// sitemap lists every post of the given types ("movies", "tvshows",
// "episodes") with its last-modified date.
func sitemap(ctx context.Context, f *fetch.Fetcher, types ...string) (map[string][]entry, error) {
	index, err := f.Get(ctx, baseURL+"/wp-sitemap.xml")
	if err != nil {
		return nil, err
	}
	out := map[string][]entry{}
	for _, m := range reSitemap.FindAllStringSubmatch(index, -1) {
		if !slices.Contains(types, m[2]) {
			continue
		}
		body, err := f.Get(ctx, m[1])
		if err != nil {
			return nil, err
		}
		for _, u := range reURL.FindAllStringSubmatch(body, -1) {
			out[m[2]] = append(out[m[2]], entry{loc: u[1], lastmod: u[2]})
		}
	}
	for _, t := range types {
		if len(out[t]) == 0 {
			return nil, fmt.Errorf("sitemap lists no %s", t)
		}
	}
	return out, nil
}

func slugOf(u string) string {
	return filepath.Base(strings.TrimSuffix(u, "/"))
}

// players asks the player API for each quality of a post. kind is "movie"
// or "tv".
func players(ctx context.Context, f *fetch.Fetcher, id int, kind string) ([]*Player, error) {
	var ps []*Player
	for n := 1; n <= maxPlayers; n++ {
		var r struct {
			EmbedURL string `json:"embed_url"`
		}
		if err := f.GetJSON(ctx, fmt.Sprintf("%s/wp-json/dooplayer/v2/%d/%s/%d", baseURL, id, kind, n), &r); err != nil {
			return nil, err
		}
		if r.EmbedURL == "" {
			break
		}
		ps = append(ps, &Player{Label: quality(r.EmbedURL), URL: r.EmbedURL})
	}
	// Best quality first.
	slices.SortStableFunc(ps, func(a, b *Player) int { return cmp.Compare(qualityRank(b.Label), qualityRank(a.Label)) })
	return ps, nil
}

func qualityRank(label string) int {
	var n int
	fmt.Sscanf(label, "%d", &n)
	return n
}

// ---- series ----

func (s *Source) SyncSeries(ctx context.Context, env *source.Env) ([]*catalog.Title, error) {
	sm, err := sitemap(ctx, env.Fetch, "tvshows", "episodes")
	if err != nil {
		return nil, err
	}
	epMod := map[string]string{}
	for _, e := range sm["episodes"] {
		epMod[e.loc] = e.lastmod
	}

	// Load what's saved, and note which show each known episode belongs to.
	entries := sm["tvshows"]
	saved := make([]*Show, len(entries))
	owner := map[string]int{}
	for i, e := range entries {
		var sh *Show
		if ok, _ := catalog.ReadJSON(filepath.Join(env.Dir, "series", slugOf(e.loc)+".json"), &sh); ok {
			saved[i] = sh
			for _, se := range sh.Seasons {
				for _, ep := range se.Episodes {
					owner[ep.URL] = i
				}
			}
		}
	}

	// A show needs its page re-read if it's new, its own date changed, one
	// of its episodes changed or disappeared, or a new episode may be its.
	dirty := make([]bool, len(entries))
	for i, e := range entries {
		sh := saved[i]
		dirty[i] = sh == nil || env.Deep || sh.LastMod != e.lastmod
		for _, se := range sh.seasons() {
			for _, ep := range se.Episodes {
				if mod, ok := epMod[ep.URL]; !ok || mod != ep.LastMod {
					dirty[i] = true
				}
			}
		}
	}
	// Some episodes in the sitemap aren't on any show page. Once a full
	// check has found that, they are remembered so they don't trigger
	// another one, until their date changes.
	orphansPath := filepath.Join(env.Dir, "orphans.json")
	orphans := map[string]string{}
	catalog.ReadJSON(orphansPath, &orphans)
	for u, mod := range epMod {
		if _, ok := owner[u]; ok {
			continue
		}
		if prev, ok := orphans[u]; ok && prev == mod && !env.Deep {
			continue
		}
		// New episodes are usually named after their show
		// ("sarzamine-madari-se03-ep01-..."); if no show matches, check them all.
		found := false
		for i, e := range entries {
			if strings.HasPrefix(slugOf(u), baseSlug(slugOf(e.loc))+"-") {
				dirty[i], found = true, true
			}
		}
		if !found {
			for i := range dirty {
				dirty[i] = true
			}
			break
		}
	}

	env.Phase("series", len(entries))
	shows := make([]*Show, len(entries))
	var failed atomic.Int32
	err = source.Parallel(ctx, 3, entries, func(i int, e entry) {
		defer env.Done()
		shows[i] = saved[i]
		if !dirty[i] {
			return
		}
		sh, err := s.syncShow(ctx, env, e, saved[i], epMod)
		if err != nil {
			if errors.Is(err, fetch.ErrNotFound) {
				return // the sitemap lists a page that doesn't exist; nothing to retry
			}
			if ctx.Err() == nil {
				failed.Add(1)
				env.Logf("%s: %v (will retry on next sync)", slugOf(e.loc), err)
			}
			return
		}
		shows[i] = sh
	})

	if err == nil && failed.Load() == 0 {
		// Every show is now current, so episodes no show lists are orphans.
		listed := map[string]bool{}
		for _, sh := range shows {
			for _, se := range sh.seasons() {
				for _, ep := range se.Episodes {
					listed[ep.URL] = true
				}
			}
		}
		orphans = map[string]string{}
		for u, mod := range epMod {
			if !listed[u] {
				orphans[u] = mod
			}
		}
		if err := catalog.WriteJSON(orphansPath, orphans); err != nil {
			env.Logf("saving orphans: %v", err)
		}
	}

	// Most recently updated first.
	shows = slices.DeleteFunc(shows, func(sh *Show) bool { return sh == nil })
	slices.SortStableFunc(shows, func(a, b *Show) int { return cmp.Compare(b.latest(), a.latest()) })
	titles := make([]*catalog.Title, len(shows))
	for i, sh := range shows {
		titles[i] = s.showTitle(sh)
	}
	if err != nil {
		return titles, err
	}
	if n := failed.Load(); n > 0 {
		return titles, fmt.Errorf("%d series failed", n)
	}
	return titles, nil
}

func (sh *Show) seasons() []*Season {
	if sh == nil {
		return nil
	}
	return sh.Seasons
}

// latest is the newest date of the show or any of its episodes.
func (sh *Show) latest() string {
	l := sh.LastMod
	for _, se := range sh.Seasons {
		for _, ep := range se.Episodes {
			l = max(l, ep.LastMod)
		}
	}
	return l
}

// syncShow re-reads a show page and fetches the players of episodes that
// are new or changed; unchanged episodes keep what was saved.
func (s *Source) syncShow(ctx context.Context, env *source.Env, e entry, old *Show, epMod map[string]string) (*Show, error) {
	page, err := env.Fetch.Get(ctx, e.loc)
	if err != nil {
		return nil, err
	}
	sh := &Show{Slug: slugOf(e.loc), URL: e.loc, LastMod: e.lastmod, Info: parseInfo(page), Seasons: parseSeasons(page)}

	known := map[string]*Episode{}
	for _, se := range old.seasons() {
		for _, ep := range se.Episodes {
			known[ep.URL] = ep
		}
	}
	var todo []*Episode
	added := 0
	for _, se := range sh.Seasons {
		for _, ep := range se.Episodes {
			ep.LastMod = epMod[ep.URL]
			prev, ok := known[ep.URL]
			if !ok {
				added++
			}
			if ok && !env.Deep && prev.LastMod == ep.LastMod && len(prev.Players) > 0 {
				ep.PostID, ep.Players = prev.PostID, prev.Players
				continue
			}
			todo = append(todo, ep)
		}
	}

	var failed atomic.Int32
	var first atomic.Value
	err = source.Parallel(ctx, 2, todo, func(_ int, ep *Episode) {
		if err := fetchEpisode(ctx, env.Fetch, ep); err != nil {
			failed.Add(1)
			first.CompareAndSwap(nil, err)
		}
	})
	if err != nil {
		return nil, err
	}
	if n := failed.Load(); n > 0 {
		return nil, fmt.Errorf("%d episodes failed, e.g. %v", n, first.Load())
	}

	b, err := catalog.Marshal(sh)
	if err != nil {
		return nil, err
	}
	if old != nil {
		if prev, err := catalog.Marshal(old); err == nil && bytes.Equal(prev, b) {
			return old, nil
		}
	}
	if err := catalog.WriteFile(filepath.Join(env.Dir, "series", sh.Slug+".json"), b); err != nil {
		return nil, err
	}
	switch {
	case old == nil:
		env.Logf("+ series %s: %d episodes", sh.Slug, added)
	case added > 0:
		env.Logf("~ series %s: %d new episode(s)", sh.Slug, added)
	default:
		env.Logf("~ series %s: updated", sh.Slug)
	}
	return sh, nil
}

// fetchEpisode reads an episode page for its post id, then its players.
func fetchEpisode(ctx context.Context, f *fetch.Fetcher, ep *Episode) error {
	if ep.PostID == 0 {
		page, err := f.Get(ctx, ep.URL)
		if errors.Is(err, fetch.ErrNotFound) {
			return nil // listed on the show page but gone; leave it unplayable
		}
		if err != nil {
			return err
		}
		if ep.PostID = postID(page); ep.PostID == 0 {
			return fmt.Errorf("%s: no post id", ep.URL)
		}
	}
	ps, err := players(ctx, f, ep.PostID, "tv")
	ep.Players = ps
	return err
}

func (s *Source) showTitle(sh *Show) *catalog.Title {
	t := s.title(sh.Slug, sh.URL, sh.Info)
	for _, se := range sh.Seasons {
		cse := &catalog.Season{Number: se.Number, Name: fmt.Sprintf("Season %d", se.Number)}
		for _, ep := range se.Episodes {
			cse.Episodes = append(cse.Episodes, &catalog.Episode{
				Number:    ep.Number,
				Title:     ep.Title,
				Thumbnail: ep.Thumbnail,
				Players:   s.players(ep.Players),
			})
		}
		slices.SortStableFunc(cse.Episodes, func(a, b *catalog.Episode) int { return cmp.Compare(a.Number, b.Number) })
		t.Seasons = append(t.Seasons, cse)
	}
	slices.SortStableFunc(t.Seasons, func(a, b *catalog.Season) int { return cmp.Compare(a.Number, b.Number) })
	return t
}

func (s *Source) title(slug, u string, in Info) *catalog.Title {
	return &catalog.Title{
		ID:          baseSlug(slug),
		Title:       cmp.Or(in.Title, baseSlug(slug)),
		TitleFa:     in.TitleFa,
		Poster:      in.Poster,
		Description: in.Description,
		Links:       []catalog.Link{{Source: s.Name(), URL: u}},
	}
}

func (s *Source) players(ps []*Player) []*catalog.Player {
	var out []*catalog.Player
	for _, p := range ps {
		out = append(out, &catalog.Player{Source: s.Name(), Label: p.Label, Type: "iframe", URL: p.URL})
	}
	return out
}

// ---- movies ----

func (s *Source) SyncMovies(ctx context.Context, env *source.Env) ([]*catalog.Title, error) {
	sm, err := sitemap(ctx, env.Fetch, "movies")
	if err != nil {
		return nil, err
	}
	entries := sm["movies"]
	env.Phase("movies", len(entries))

	movies := make([]*Movie, len(entries))
	var failed atomic.Int32
	err = source.Parallel(ctx, 3, entries, func(i int, e entry) {
		defer env.Done()
		path := filepath.Join(env.Dir, "movies", slugOf(e.loc)+".json")
		var old *Movie
		if ok, _ := catalog.ReadJSON(path, &old); !ok {
			old = nil
		}
		movies[i] = old
		if old != nil && !env.Deep && old.LastMod == e.lastmod && len(old.Players) > 0 {
			return
		}
		m, err := fetchMovie(ctx, env.Fetch, e, old)
		if err == nil {
			err = catalog.WriteJSON(path, m)
		}
		if err != nil {
			if errors.Is(err, fetch.ErrNotFound) {
				return // the sitemap lists a page that doesn't exist; nothing to retry
			}
			if ctx.Err() == nil {
				failed.Add(1)
				env.Logf("%s: %v (will retry on next sync)", slugOf(e.loc), err)
			}
			return
		}
		movies[i] = m
		if old == nil {
			env.Logf("+ movie %s", m.Slug)
		} else {
			env.Logf("~ movie %s: updated", m.Slug)
		}
	})

	movies = slices.DeleteFunc(movies, func(m *Movie) bool { return m == nil })
	slices.SortStableFunc(movies, func(a, b *Movie) int { return cmp.Compare(b.LastMod, a.LastMod) })
	titles := make([]*catalog.Title, len(movies))
	for i, m := range movies {
		titles[i] = s.title(m.Slug, m.URL, m.Info)
		titles[i].Players = s.players(m.Players)
	}
	if err != nil {
		return titles, err
	}
	if n := failed.Load(); n > 0 {
		return titles, fmt.Errorf("%d movies failed", n)
	}
	return titles, nil
}

func fetchMovie(ctx context.Context, f *fetch.Fetcher, e entry, old *Movie) (*Movie, error) {
	page, err := f.Get(ctx, e.loc)
	if err != nil {
		return nil, err
	}
	m := &Movie{Slug: slugOf(e.loc), URL: e.loc, LastMod: e.lastmod, PostID: postID(page), Info: parseInfo(page)}
	if m.PostID == 0 {
		return nil, fmt.Errorf("no post id")
	}
	if m.Players, err = players(ctx, f, m.PostID, "movie"); err != nil {
		return nil, err
	}
	if len(m.Players) == 0 && old != nil {
		m.Players = old.Players
	}
	return m, nil
}
