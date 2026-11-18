// Package rsync runs syncs, tracks their status, and merges each section's
// sources into the catalog the site serves.
//
// Sources are grouped into sections (farsi, foreign). Each section syncs on
// its own: at most one full sync per section runs at a time, and at most one
// sync per title (a second request for the same title waits for the first).
// Everything is kept in one SQLite database, <dir>/movies.db (see package
// store).
package rsync

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"farsi.ella.to/internal/catalog"
	"farsi.ella.to/internal/fetch"
	"farsi.ella.to/internal/source"
	"farsi.ella.to/internal/store"
)

type State string

const (
	Idle    State = "IDLE"    // no sync has run yet
	Running State = "RUNNING" // sources are syncing
	Merging State = "MERGING" // sources are done; building the merged catalog
	Done    State = "DONE"
	Failed  State = "FAILED"
)

var (
	ErrUnknownSection = errors.New("unknown section")
	ErrNotFound       = errors.New("title not found")
	// ErrBusy is returned when a title sync is asked for while its section's
	// full sync is running (which updates every title anyway).
	ErrBusy = errors.New("a full sync of this section is running")
)

// maxLog is how many recent events the status keeps.
const maxLog = 200

type Status struct {
	Section    string     `json:"section"`
	State      State      `json:"state"`
	Deep       bool       `json:"deep,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	// Series and Movies count the merged catalog being served.
	Series  int             `json:"series"`
	Movies  int             `json:"movies"`
	Sources []*SourceStatus `json:"sources"`
	// Titles lists the title syncs running now ("series/<id>").
	Titles []string `json:"titles,omitempty"`
	Log    []string `json:"log"` // recent events, oldest first
}

type SourceStatus struct {
	Name       string     `json:"name"`
	State      State      `json:"state"`
	Phase      string     `json:"phase,omitempty"` // "series" or "movies"
	Done       int        `json:"done"`            // items finished in this phase
	Total      int        `json:"total"`
	Requests   int64      `json:"requests"`
	Series     int        `json:"series"`
	Movies     int        `json:"movies"`
	Errors     []string   `json:"errors,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Data is a section's title list (see Summary), gzipped, ready to serve.
type Data struct {
	Gzip []byte
	ETag string
}

// Summary is what a list shows of a title; the rest comes from Title.
type Summary struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	TitleFa string `json:"title_fa,omitempty"`
	Poster  string `json:"poster,omitempty"`
	Eps     int    `json:"eps,omitempty"`     // playable episodes of a series
	Partial bool   `json:"partial,omitempty"` // details not fetched yet
}

type Syncer struct {
	store    *store.Store
	fetchers map[string]*fetch.Fetcher // one per source, shared by all its syncs

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	sections map[string]*Section
	names    []string
}

// Section is one part of the site with its own sources, syncs and catalog.
type Section struct {
	s       *Syncer
	name    string
	sources []source.Source

	mu       sync.Mutex
	status   Status
	baseReqs map[string]int64                     // each fetcher's count when the sync started
	cats     map[string]*catalog.Catalog          // each source's catalog
	merged   *catalog.Catalog                     // all of them merged
	byID     map[string]map[string]*catalog.Title // kind -> id -> merged title
	calls    map[string]*call                     // running title syncs, by kind/id

	mergeMu sync.Mutex // one merge at a time, so a stale one never wins
	data    atomic.Pointer[Data]
}

type call struct {
	done chan struct{}
	t    *catalog.Title
	err  error
}

// New opens the database in dir (importing the JSON files of earlier
// versions on first run), restores each section's status, and builds the
// merged catalogs from what the sources have saved.
func New(dir string, sources ...source.Source) (*Syncer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	db, err := store.Open(ctx, filepath.Join(dir, "movies.db"))
	if err != nil {
		cancel()
		return nil, err
	}
	s := &Syncer{store: db, fetchers: map[string]*fetch.Fetcher{}, ctx: ctx, cancel: cancel, sections: map[string]*Section{}}

	if empty, err := db.Empty(ctx); err == nil && empty {
		var names []string
		for _, src := range sources {
			names = append(names, src.Name())
		}
		if n, err := db.ImportFiles(ctx, dir, names); err != nil {
			log.Printf("importing the JSON files in %s: %v", dir, err)
		} else if n > 0 {
			log.Printf("imported %d titles from the JSON files in %s into the database", n, dir)
		}
		// The old status.json was the Farsi sources' (the only ones then).
		var st Status
		if ok, err := catalog.ReadJSON(filepath.Join(dir, "status.json"), &st); err == nil && ok {
			st.Section = source.Farsi
			if err := db.PutStatus(ctx, source.Farsi, &st); err != nil {
				log.Printf("importing status.json: %v", err)
			}
		}
	}

	for _, src := range sources {
		s.fetchers[src.Name()] = fetch.New(src.RPS(), 4)
		sec := s.sections[src.Section()]
		if sec == nil {
			sec = &Section{s: s, name: src.Section(), cats: map[string]*catalog.Catalog{}, calls: map[string]*call{}}
			s.sections[sec.name] = sec
			s.names = append(s.names, sec.name)
		}
		sec.sources = append(sec.sources, src)
	}

	for _, name := range s.names {
		sec := s.sections[name]
		if err := sec.restore(); err != nil {
			s.Close()
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	return s, nil
}

// restore loads the section's saved status and catalogs and merges them.
func (sec *Section) restore() error {
	ctx := sec.s.ctx
	sec.status = Status{Section: sec.name, State: Idle}
	if _, err := sec.s.store.Status(ctx, sec.name, &sec.status); err != nil {
		log.Printf("[%s] ignoring the saved status: %v", sec.name, err)
		sec.status = Status{Section: sec.name, State: Idle}
	}
	sec.status.Section, sec.status.Titles = sec.name, nil
	if sec.status.State == Running || sec.status.State == Merging {
		sec.status.State = Failed
		sec.status.Error = "interrupted: the server stopped during the sync (the next sync resumes)"
		for _, ss := range sec.status.Sources {
			if ss.State == Running {
				ss.State = Failed
			}
		}
	}
	for _, src := range sec.sources {
		cat := &catalog.Catalog{}
		var err error
		if cat.Series, err = sec.s.store.Titles(ctx, src.Name(), source.Series); err != nil {
			return err
		}
		if cat.Movies, err = sec.s.store.Titles(ctx, src.Name(), source.Movies); err != nil {
			return err
		}
		sec.cats[src.Name()] = cat
	}
	return sec.merge()
}

// Close stops running syncs, waits for them to wind down, and closes the
// database. Progress is saved as it goes, so an interrupted sync resumes.
func (s *Syncer) Close() {
	s.cancel()
	s.wg.Wait()
	for _, f := range s.fetchers {
		f.Close()
	}
	if err := s.store.Close(); err != nil {
		log.Printf("closing the database: %v", err)
	}
}

// Sections lists the section names, in the order of their first source.
func (s *Syncer) Sections() []string { return s.names }

// Section returns a section by name.
func (s *Syncer) Section(name string) (*Section, error) {
	if sec := s.sections[name]; sec != nil {
		return sec, nil
	}
	return nil, fmt.Errorf("%w %q", ErrUnknownSection, name)
}

// Data returns the section's title list.
func (sec *Section) Data() *Data { return sec.data.Load() }

// Status returns a snapshot of the section's status as JSON.
func (sec *Section) Status() []byte {
	sec.mu.Lock()
	defer sec.mu.Unlock()
	if sec.status.State == Running {
		for _, ss := range sec.status.Sources {
			if f := sec.s.fetchers[ss.Name]; f != nil {
				ss.Requests = f.Requests.Load() - sec.baseReqs[ss.Name]
			}
		}
	}
	sec.status.Titles = sec.status.Titles[:0]
	for k := range sec.calls {
		sec.status.Titles = append(sec.status.Titles, k)
	}
	slices.Sort(sec.status.Titles)
	b, _ := json.Marshal(&sec.status)
	return b
}

func (sec *Section) running() bool {
	return sec.status.State == Running || sec.status.State == Merging
}

// Start begins a full sync of the section in the background. It returns
// false if one is already running. Deep re-checks every title instead of
// trusting the sites' listings.
func (sec *Section) Start(deep bool) bool {
	sec.mu.Lock()
	defer sec.mu.Unlock()
	if sec.running() || sec.s.ctx.Err() != nil {
		return false
	}
	now := time.Now().UTC()
	sec.status = Status{
		Section:   sec.name,
		State:     Running,
		Deep:      deep,
		StartedAt: &now,
		Series:    sec.status.Series,
		Movies:    sec.status.Movies,
		Log:       sec.status.Log,
	}
	sec.baseReqs = map[string]int64{}
	for _, src := range sec.sources {
		sec.status.Sources = append(sec.status.Sources, &SourceStatus{Name: src.Name(), State: Running, StartedAt: &now})
		sec.baseReqs[src.Name()] = sec.s.fetchers[src.Name()].Requests.Load()
	}
	sec.logf("sync started (deep=%v)", deep)
	sec.saveStatus()

	sec.s.wg.Go(func() { sec.run(deep) })
	return true
}

func (sec *Section) run(deep bool) {
	var wg sync.WaitGroup
	for i, src := range sec.sources {
		wg.Go(func() { sec.syncSource(src, sec.status.Sources[i], deep) })
	}
	wg.Wait()

	sec.mu.Lock()
	sec.status.State = Merging
	sec.mu.Unlock()

	err := sec.merge()

	sec.mu.Lock()
	defer sec.mu.Unlock()
	now := time.Now().UTC()
	sec.status.FinishedAt = &now
	failed := 0
	for _, ss := range sec.status.Sources {
		ss.Requests = sec.s.fetchers[ss.Name].Requests.Load() - sec.baseReqs[ss.Name]
		if ss.State == Failed {
			failed++
		}
	}
	switch {
	case sec.s.ctx.Err() != nil:
		sec.status.State, sec.status.Error = Failed, "interrupted: the server stopped during the sync (the next sync resumes)"
	case err != nil:
		sec.status.State, sec.status.Error = Failed, "merge: "+err.Error()
	case failed == len(sec.sources):
		sec.status.State, sec.status.Error = Failed, "every source failed"
	default:
		sec.status.State = Done
	}
	sec.logf("sync %s in %s: %d series, %d movies", sec.status.State, now.Sub(*sec.status.StartedAt).Round(time.Second), sec.status.Series, sec.status.Movies)
	sec.saveStatus()
}

func (sec *Section) env(src source.Source, deep bool, r source.Report) *source.Env {
	return &source.Env{Store: sec.s.store.Bucket(src.Name()), Fetch: sec.s.fetchers[src.Name()], Deep: deep, Report: r}
}

// syncSource syncs one source's series, then its movies, and saves its
// catalog. A part that fails outright keeps the series or movies of the last
// good catalog. One that fails for some titles returns all of them (failed
// ones as last saved), so its list is used. An interrupted part returns only
// the titles it got to, so the last good list is kept.
func (sec *Section) syncSource(src source.Source, ss *SourceStatus, deep bool) {
	ctx := sec.s.ctx
	env := sec.env(src, deep, &reporter{sec: sec, ss: ss})

	sec.mu.Lock()
	old := sec.cats[src.Name()]
	sec.mu.Unlock()
	cat := &catalog.Catalog{Series: old.Series, Movies: old.Movies}

	var errs []error
	series, err := src.SyncSeries(ctx, env)
	if err != nil {
		errs = append(errs, fmt.Errorf("series: %w", err))
	}
	if err == nil || (len(series) > 0 && ctx.Err() == nil) {
		cat.Series = series
		if err := sec.s.store.ReplaceTitles(ctx, src.Name(), source.Series, series); err != nil {
			errs = append(errs, err)
		}
	}
	if ctx.Err() == nil {
		movies, err := src.SyncMovies(ctx, env)
		if err != nil {
			errs = append(errs, fmt.Errorf("movies: %w", err))
		}
		if err == nil || (len(movies) > 0 && ctx.Err() == nil) {
			cat.Movies = movies
			if err := sec.s.store.ReplaceTitles(ctx, src.Name(), source.Movies, movies); err != nil {
				errs = append(errs, err)
			}
		}
	}
	cat.UpdatedAt = time.Now().UTC()

	sec.mu.Lock()
	defer sec.mu.Unlock()
	sec.cats[src.Name()] = cat
	now := time.Now().UTC()
	ss.FinishedAt = &now
	ss.Series, ss.Movies = len(cat.Series), len(cat.Movies)
	ss.Phase = ""
	ss.State = Done
	for _, err := range errs {
		ss.State = Failed
		ss.Errors = append(ss.Errors, err.Error())
		sec.logf("[%s] %v", src.Name(), err)
	}
	sec.logf("[%s] %s: %d series, %d movies", src.Name(), ss.State, ss.Series, ss.Movies)
}

// merge combines the sources' catalogs into the one the section serves.
func (sec *Section) merge() error {
	sec.mergeMu.Lock()
	defer sec.mergeMu.Unlock()

	sec.mu.Lock()
	var cats []*catalog.Catalog
	for _, src := range sec.sources {
		if c := sec.cats[src.Name()]; c != nil {
			cats = append(cats, c)
		}
	}
	merged := catalog.Merge(cats...) // copies, so the sources' catalogs can change after
	byID := map[string]map[string]*catalog.Title{source.Series: {}, source.Movies: {}}
	for _, t := range merged.Series {
		byID[source.Series][t.ID] = t
	}
	for _, t := range merged.Movies {
		byID[source.Movies][t.ID] = t
	}
	sec.merged, sec.byID = merged, byID
	sec.status.Series, sec.status.Movies = len(merged.Series), len(merged.Movies)
	sec.mu.Unlock()

	list := struct {
		Series []*Summary `json:"series"`
		Movies []*Summary `json:"movies"`
	}{summaries(merged.Series, true), summaries(merged.Movies, false)}

	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(list); err != nil {
		return err
	}
	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(raw.Bytes())
	if err := zw.Close(); err != nil {
		return err
	}
	sum := sha256.Sum256(raw.Bytes())
	sec.data.Store(&Data{Gzip: gz.Bytes(), ETag: `"` + hex.EncodeToString(sum[:8]) + `"`})
	log.Printf("[%s] merged %d source(s): %d series, %d movies (%d KB gzipped)", sec.name, len(cats), len(merged.Series), len(merged.Movies), gz.Len()/1024)
	return nil
}

func summaries(ts []*catalog.Title, series bool) []*Summary {
	out := make([]*Summary, len(ts))
	for i, t := range ts {
		s := &Summary{ID: t.ID, Title: t.Title, TitleFa: t.TitleFa, Poster: t.Poster, Partial: t.Partial}
		if series && !t.Partial {
			s.Eps = t.Playable()
		}
		out[i] = s
	}
	return out
}

// Title returns a merged title by kind ("series" or "movies") and id.
func (sec *Section) Title(kind, id string) *catalog.Title {
	sec.mu.Lock()
	defer sec.mu.Unlock()
	return sec.byID[kind][id]
}

// SyncTitle syncs one title on every source it is on and returns the
// updated title. A title already being synced isn't synced twice: the call
// waits for the running one. Explicit (user-requested) syncs are refused
// with ErrBusy while the section's full sync runs; implicit ones (fetching a
// partial title's details) always run. The sync continues if ctx is
// cancelled; only the wait stops.
func (sec *Section) SyncTitle(ctx context.Context, kind, id string, deep, explicit bool) (*catalog.Title, error) {
	key := kind + "/" + id
	sec.mu.Lock()
	if explicit && sec.running() {
		sec.mu.Unlock()
		return nil, ErrBusy
	}
	t := sec.byID[kind][id]
	if t == nil {
		sec.mu.Unlock()
		return nil, ErrNotFound
	}
	c := sec.calls[key]
	if c == nil {
		c = &call{done: make(chan struct{})}
		sec.calls[key] = c
		links := slices.Clone(t.Links)
		title := t.Title
		sec.s.wg.Go(func() {
			c.t, c.err = sec.syncTitle(kind, title, links, deep)
			sec.mu.Lock()
			delete(sec.calls, key)
			sec.mu.Unlock()
			close(c.done)
		})
	}
	sec.mu.Unlock()

	select {
	case <-c.done:
		return c.t, c.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (sec *Section) syncTitle(kind, title string, links []catalog.Link, deep bool) (*catalog.Title, error) {
	ctx := sec.s.ctx
	urls := map[string]bool{}
	for _, l := range links {
		urls[l.URL] = true
	}
	sec.withLog(func() { sec.logf("syncing %s %q", kind, title) })

	var errs []error
	synced := 0
	for _, src := range sec.sources {
		if source.LinkOf(&catalog.Title{Links: links}, src.Name()) == "" {
			continue
		}
		// This source's own version of the title, found by its page URL.
		sec.mu.Lock()
		list := sec.cats[src.Name()].Series
		if kind == source.Movies {
			list = sec.cats[src.Name()].Movies
		}
		idx := slices.IndexFunc(list, func(t *catalog.Title) bool {
			return slices.ContainsFunc(t.Links, func(l catalog.Link) bool { return urls[l.URL] })
		})
		own := &catalog.Title{Title: title}
		if idx >= 0 {
			own = list[idx]
		} else {
			for _, l := range links {
				if l.Source == src.Name() {
					own.Links = append(own.Links, l)
				}
			}
		}
		sec.mu.Unlock()

		nt, err := src.SyncTitle(ctx, sec.env(src, deep, &titleReporter{sec: sec, src: src.Name()}), kind, own)
		if err == nil {
			err = sec.s.store.PutTitle(ctx, src.Name(), kind, nt)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
			continue
		}
		synced++

		sec.mu.Lock()
		cat := sec.cats[src.Name()]
		list = cat.Series
		if kind == source.Movies {
			list = cat.Movies
		}
		// Replace by the title's own URL: the full sync may have swapped the list.
		key := own.Links
		if len(key) == 0 {
			key = nt.Links
		}
		i := slices.IndexFunc(list, func(t *catalog.Title) bool {
			return len(t.Links) > 0 && len(key) > 0 && t.Links[0].URL == key[0].URL
		})
		list = slices.Clone(list)
		if i >= 0 {
			list[i] = nt
		} else {
			list = append(list, nt)
		}
		if kind == source.Movies {
			cat.Movies = list
		} else {
			cat.Series = list
		}
		sec.mu.Unlock()
	}

	err := errors.Join(errs...)
	sec.withLog(func() {
		if err != nil {
			sec.logf("%s %q: %v", kind, title, err)
		} else {
			sec.logf("%s %q synced", kind, title)
		}
	})
	if synced == 0 {
		if err == nil {
			err = errors.New("no source to sync it from")
		}
		return nil, err
	}
	if merr := sec.merge(); merr != nil {
		return nil, merr
	}

	// The merged title may have a new id if its name changed: find it by URL.
	sec.mu.Lock()
	defer sec.mu.Unlock()
	list := sec.merged.Series
	if kind == source.Movies {
		list = sec.merged.Movies
	}
	for _, t := range list {
		if slices.ContainsFunc(t.Links, func(l catalog.Link) bool { return urls[l.URL] }) {
			return t, err
		}
	}
	return nil, ErrNotFound
}

func (sec *Section) withLog(fn func()) {
	sec.mu.Lock()
	defer sec.mu.Unlock()
	fn()
	if !sec.running() {
		sec.saveStatus()
	}
}

// logf adds an event to the status log. sec.mu must be held.
func (sec *Section) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("[%s] %s", sec.name, msg)
	sec.status.Log = append(sec.status.Log, time.Now().UTC().Format("15:04:05")+" "+msg)
	if n := len(sec.status.Log) - maxLog; n > 0 {
		sec.status.Log = sec.status.Log[n:]
	}
}

// saveStatus writes the status to the database. sec.mu must be held.
func (sec *Section) saveStatus() {
	st := sec.status
	st.Titles = nil
	if err := sec.s.store.PutStatus(context.Background(), sec.name, &st); err != nil {
		log.Printf("[%s] saving status: %v", sec.name, err)
	}
}

// reporter feeds one source's progress into the status.
type reporter struct {
	sec *Section
	ss  *SourceStatus
}

func (r *reporter) Phase(name string, total int) {
	r.sec.mu.Lock()
	defer r.sec.mu.Unlock()
	r.ss.Phase, r.ss.Done, r.ss.Total = name, 0, total
}

func (r *reporter) Done() {
	r.sec.mu.Lock()
	defer r.sec.mu.Unlock()
	r.ss.Done++
}

func (r *reporter) Logf(format string, args ...any) {
	r.sec.mu.Lock()
	defer r.sec.mu.Unlock()
	r.sec.logf("["+r.ss.Name+"] "+format, args...)
}

// titleReporter logs a title sync's events; it has no progress to show.
type titleReporter struct {
	sec *Section
	src string
}

func (*titleReporter) Phase(string, int) {}
func (*titleReporter) Done()             {}
func (r *titleReporter) Logf(format string, args ...any) {
	r.sec.mu.Lock()
	defer r.sec.mu.Unlock()
	r.sec.logf("["+r.src+"] "+format, args...)
}
