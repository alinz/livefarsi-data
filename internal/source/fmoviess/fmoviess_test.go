package fmoviess

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"farsi.ella.to/internal/catalog"
	"farsi.ella.to/internal/fetch"
	"farsi.ella.to/internal/source"
)

// cardHTML is a card as the site writes it (see testdata/movies.html).
func cardHTML(slug, title, badge string) string {
	return fmt.Sprintf(`<div class=col><div class="card bg-transparent border-0 h-100"><a href=%s/film/%s/ class="rounded poster">`+
		`<picture><img src="data:image/svg+xml;base64,AAAA" data-src=https://img.example/%s.jpg class="lazy card-img" alt="%s"></picture>`+
		`<div class="card-body item-title"><h2 class="card-title">%s</h2></div><span class=mlbq>%s</span></a></div></div>`, baseURL, slug, slug, title, title, badge)
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseListing(t *testing.T) {
	cards, last := parseListing(readFile(t, "tv-series.html"))
	if last != 248 {
		t.Errorf("last = %d, want 248", last)
	}
	want := []card{
		{slug: "wandavision-1-101990", url: baseURL + "/film/wandavision-1-101990/", title: "WandaVision - Season 1", poster: "https://img.cdno.my.id/thumb/w_312/h_468/wandavision-1-101990.jpg", badge: "Eps 9"},
	}
	if len(cards) != 3 || cards[0] != want[0] {
		t.Errorf("cards =\n%+v\nwant 3, the first\n%+v", cards, want[0])
	}

	cards, last = parseListing(readFile(t, "movies.html"))
	if last != 684 || len(cards) != 3 {
		t.Fatalf("movies: last = %d, %d cards", last, len(cards))
	}
	if c := cards[1]; c.slug != "insecta-5397" || c.title != "Insecta" || c.badge != "HD" || c.poster != "https://img.cdno.my.id/thumb/w_312/h_468/insecta-5397.jpg" {
		t.Errorf("unquoted alt: %+v", c)
	}
}

func TestSplitSeason(t *testing.T) {
	for _, c := range []struct {
		in   string
		show string
		n    int
	}{
		{"WandaVision - Season 1", "WandaVision", 1},
		{"The Office: Season 12", "The Office", 12},
		{"Insecta", "Insecta", 1},
	} {
		if show, n := splitSeason(c.in); show != c.show || n != c.n {
			t.Errorf("splitSeason(%q) = %q, %d; want %q, %d", c.in, show, n, c.show, c.n)
		}
	}
}

func TestParsePage(t *testing.T) {
	title, _, d := parsePage(readFile(t, "film-movie.html"))
	if title != "5 Centimeters per Second" || d.MID != 5398 || d.Mode != "movie" {
		t.Errorf("movie: %q, mid %d, mode %q", title, d.MID, d.Mode)
	}
	if !strings.HasPrefix(d.Description, "After transferring to the same elementary school") ||
		d.Year != 2025 || d.Duration != "2h 1m" || !slices.Equal(d.Genres, []string{"Drama", "Romance"}) {
		t.Errorf("movie detail = %+v", d)
	}
	if !slices.Equal(d.Servers, []string{"Server 1", "Server 2", "Server 3"}) || len(d.Episodes) != 1 || d.Episodes[0].Title != "Full HD" {
		t.Errorf("movie servers %v, episodes %+v", d.Servers, d.Episodes)
	}

	title, poster, d := parsePage(readFile(t, "film-series.html"))
	if title != "WandaVision - Season 1" || d.MID != 101990 || d.Mode != "serie" || d.Year != 2021 || d.Duration != "31m" {
		t.Errorf("series: %q %+v", title, d)
	}
	if poster == "" {
		t.Error("series: no poster from og:image")
	}
	if len(d.Episodes) != 9 || d.Episodes[8].Number != 9 || d.Episodes[8].Title != "Episode 9" {
		t.Errorf("series episodes = %d, last %+v", len(d.Episodes), d.Episodes[len(d.Episodes)-1])
	}
}

// memStore is an in-memory source.Store.
type memStore struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (s *memStore) Load(_ context.Context, kind, slug string, v any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.m[kind+"/"+slug]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal(b, v)
}

func (s *memStore) Save(_ context.Context, kind, slug string, v any) error {
	b, err := json.Marshal(v)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[kind+"/"+slug] = b
	return err
}

func (s *memStore) Delete(_ context.Context, kind, slug string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, kind+"/"+slug)
	return nil
}

func (s *memStore) Each(_ context.Context, kind string, fn func(string, []byte) error) error {
	s.mu.Lock()
	var keys []string
	for k := range s.m {
		if strings.HasPrefix(k, kind+"/") {
			keys = append(keys, k)
		}
	}
	s.mu.Unlock()
	slices.Sort(keys)
	for _, k := range keys {
		if err := fn(strings.TrimPrefix(k, kind+"/"), s.m[k]); err != nil {
			return err
		}
	}
	return nil
}

type nopReport struct{}

func (nopReport) Phase(string, int)   {}
func (nopReport) Done()               {}
func (nopReport) Logf(string, ...any) {}

// fakeSite serves a three-page movie listing.
type fakeSite struct {
	mu     sync.Mutex
	pages  [][]string // slugs per page
	fail   int        // page that answers 403, 0 for none
	served []string   // paths requested
}

func (f *fakeSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.served = append(f.served, r.URL.Path)
	n := 1
	fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/movies//"), "%d", &n)
	if n == f.fail {
		http.Error(w, "nope", http.StatusForbidden)
		return
	}
	if n > len(f.pages) {
		http.NotFound(w, r)
		return
	}
	var b strings.Builder
	for _, slug := range f.pages[n-1] {
		b.WriteString(cardHTML(slug, slug, "HD"))
	}
	fmt.Fprintf(&b, `<a href="/movies//%d/">»»</a>`, len(f.pages))
	w.Write([]byte(b.String()))
}

func (f *fakeSite) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.served
	f.served = nil
	return s
}

func ids(ts []*catalog.Title) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func TestSyncResumesAndCatchesUp(t *testing.T) {
	site := &fakeSite{pages: [][]string{{"a-1", "b-2"}, {"c-3", "d-4"}, {"e-5", "f-6"}}, fail: 3}
	srv := httptest.NewServer(site)
	defer srv.Close()
	defer func(u string) { baseURL = u }(baseURL)
	baseURL = srv.URL

	f := fetch.New(1000, 0)
	defer f.Close()
	env := &source.Env{Store: &memStore{m: map[string][]byte{}}, Fetch: f, Report: nopReport{}}
	src := New()
	ctx := context.Background()

	// First sync fails on page 3, keeping pages 1 and 2.
	titles, err := src.SyncMovies(ctx, env)
	if err == nil {
		t.Fatal("first sync: want an error from page 3")
	}
	if got := ids(titles); !slices.Equal(got, []string{"a-1", "b-2", "c-3", "d-4"}) {
		t.Errorf("after failure: %v", got)
	}
	site.take()

	// The next one resumes at page 3.
	site.fail = 0
	titles, err = src.SyncMovies(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := site.take(); !slices.Equal(got, []string{"/movies//3/"}) {
		t.Errorf("resumed sync fetched %v, want only page 3", got)
	}
	if got := ids(titles); !slices.Equal(got, []string{"a-1", "b-2", "c-3", "d-4", "e-5", "f-6"}) {
		t.Errorf("after resume: %v", got)
	}
	if !titles[0].Partial || titles[0].Players != nil {
		t.Error("a listed title should be partial, without players, until its page is synced")
	}

	// A new title on top: an incremental sync reads until a page has nothing new.
	site.pages = [][]string{{"new-7", "a-1"}, {"b-2", "c-3"}, {"d-4", "e-5"}, {"f-6"}}
	titles, err = src.SyncMovies(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := site.take(); !slices.Equal(got, []string{"/movies/", "/movies//2/"}) {
		t.Errorf("incremental sync fetched %v", got)
	}
	if got := ids(titles); !slices.Equal(got, []string{"new-7", "a-1", "b-2", "c-3", "d-4", "e-5", "f-6"}) {
		t.Errorf("after incremental: %v", got)
	}
}

func TestSeriesGrouping(t *testing.T) {
	st := &memStore{m: map[string][]byte{}}
	env := &source.Env{Store: st, Report: nopReport{}}
	ctx := context.Background()
	for i, it := range []*Item{
		{Slug: "wandavision-1-101990", Title: "WandaVision - Season 1"},
		{Slug: "loki-season-2-9", Title: "Loki - Season 2"},
		{Slug: "loki-season-1-8", Title: "Loki - Season 1"},
		{Slug: "the-office-season-1-1", Title: "The Office - Season 1"},
		{Slug: "the-office-season-2-2", Title: "The Office - Season 2"},
		{Slug: "the-office-season-1-3", Title: "The Office - Season 1"}, // another show
	} {
		it.Sort = int64(i)
		it.URL = baseURL + "/film/" + it.Slug + "/"
		it.Show, it.Season = splitSeason(it.Title)
		st.Save(ctx, source.Series, it.Slug, it)
	}
	titles, err := New().titles(ctx, env, source.Series)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"wandavision", "loki", "the-office-season-1", "the-office-season-2", "the-office-season-1"}
	if got := ids(titles); !slices.Equal(got, want) {
		t.Errorf("ids = %v, want %v", got, want)
	}
	if loki := titles[1]; len(loki.Links) != 2 || loki.Links[0].URL != baseURL+"/film/loki-season-1-8/" {
		t.Errorf("loki links = %v, want season 1 first", loki.Links)
	}
}
