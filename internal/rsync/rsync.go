// Package rsync runs a sync of every source, tracks its status, and merges
// the sources into the catalog the site serves.
//
// Everything lives under one data directory:
//
//	<dir>/<source>/...           the source's own working files
//	<dir>/<source>/catalog.json  the source's last good catalog
//	<dir>/data.json              all sources merged, duplicates removed
//	<dir>/status.json            the last sync's status
package rsync

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"farsi.ella.to/internal/catalog"
	"farsi.ella.to/internal/fetch"
	"farsi.ella.to/internal/source"
)

type State string

const (
	Idle    State = "IDLE"    // no sync has run yet
	Running State = "RUNNING" // sources are syncing
	Merging State = "MERGING" // sources are done; building data.json
	Done    State = "DONE"
	Failed  State = "FAILED"
)

// maxLog is how many recent events the status keeps.
const maxLog = 200

type Status struct {
	State      State      `json:"state"`
	Deep       bool       `json:"deep,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
	// Series and Movies count the merged catalog being served.
	Series  int             `json:"series"`
	Movies  int             `json:"movies"`
	Sources []*SourceStatus `json:"sources"`
	Log     []string        `json:"log"` // recent events, oldest first
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

// Data is the merged catalog, gzipped, ready to serve.
type Data struct {
	Gzip    []byte
	ETag    string
	Updated time.Time
}

type Syncer struct {
	dir     string
	sources []source.Source

	ctx    context.Context // cancelled by Close
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	status   Status
	fetchers map[string]*fetch.Fetcher // of the running sync, for request counts

	data atomic.Pointer[Data]
}

// New prepares dir, restores the last status, and builds the merged catalog
// from whatever the sources have saved.
func New(dir string, sources ...source.Source) (*Syncer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Syncer{dir: dir, sources: sources, ctx: ctx, cancel: cancel, status: Status{State: Idle}}

	if _, err := catalog.ReadJSON(s.statusPath(), &s.status); err != nil {
		log.Printf("ignoring %s: %v", s.statusPath(), err)
		s.status = Status{State: Idle}
	}
	if s.status.State == Running || s.status.State == Merging {
		s.status.State = Failed
		s.status.Error = "interrupted: the server stopped during the sync"
		for _, ss := range s.status.Sources {
			if ss.State == Running {
				ss.State = Failed
			}
		}
	}
	// Merge on start-up too, so data.json always reflects the current
	// sources and merge rules.
	if err := s.merge(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close stops a running sync and waits for it to wind down. Files are
// written atomically, so an interrupted sync resumes where it left off.
func (s *Syncer) Close() {
	s.cancel()
	s.wg.Wait()
}

func (s *Syncer) statusPath() string { return filepath.Join(s.dir, "status.json") }

// Data returns the merged catalog.
func (s *Syncer) Data() *Data { return s.data.Load() }

// Status returns a snapshot of the current status as JSON.
func (s *Syncer) Status() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ss := range s.status.Sources {
		if f := s.fetchers[ss.Name]; f != nil {
			ss.Requests = f.Requests.Load()
		}
	}
	b, _ := json.Marshal(&s.status)
	return b
}

// Start begins a sync in the background. It returns false if one is already
// running. Deep re-checks every title instead of trusting the sites' listings.
func (s *Syncer) Start(deep bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.State == Running || s.status.State == Merging || s.ctx.Err() != nil {
		return false
	}
	now := time.Now().UTC()
	s.status = Status{
		State:     Running,
		Deep:      deep,
		StartedAt: &now,
		Series:    s.status.Series,
		Movies:    s.status.Movies,
		Log:       s.status.Log,
	}
	s.fetchers = map[string]*fetch.Fetcher{}
	for _, src := range s.sources {
		s.status.Sources = append(s.status.Sources, &SourceStatus{Name: src.Name(), State: Running, StartedAt: &now})
		s.fetchers[src.Name()] = fetch.New(src.RPS(), 4)
	}
	s.logf("sync started (deep=%v)", deep)
	s.saveStatus()

	s.wg.Go(func() { s.run(deep) })
	return true
}

func (s *Syncer) run(deep bool) {
	var wg sync.WaitGroup
	for i, src := range s.sources {
		wg.Go(func() { s.syncSource(src, s.status.Sources[i], deep) })
	}
	wg.Wait()

	s.mu.Lock()
	s.status.State = Merging
	s.mu.Unlock()

	err := s.merge()

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.fetchers {
		f.Close()
	}
	now := time.Now().UTC()
	s.status.FinishedAt = &now
	failed := 0
	for _, ss := range s.status.Sources {
		if ss.State == Failed {
			failed++
		}
	}
	switch {
	case s.ctx.Err() != nil:
		s.status.State, s.status.Error = Failed, "interrupted: the server stopped during the sync"
	case err != nil:
		s.status.State, s.status.Error = Failed, "merge: "+err.Error()
	case failed == len(s.sources):
		s.status.State, s.status.Error = Failed, "every source failed"
	default:
		s.status.State = Done
	}
	s.logf("sync %s in %s: %d series, %d movies", s.status.State, now.Sub(*s.status.StartedAt).Round(time.Second), s.status.Series, s.status.Movies)
	s.saveStatus()
}

// syncSource syncs one source's series, then its movies, and saves its
// catalog. A part that fails outright keeps the series or movies of the last
// good catalog. One that fails for some titles returns all of them (failed
// ones as last saved), so its list is used. An interrupted part returns only
// the titles it got to, so the last good list is kept.
func (s *Syncer) syncSource(src source.Source, ss *SourceStatus, deep bool) {
	dir := filepath.Join(s.dir, src.Name())
	env := &source.Env{Dir: dir, Fetch: s.fetchers[src.Name()], Deep: deep, Report: &reporter{s: s, ss: ss}}

	var errs []error
	cat := &catalog.Catalog{}
	path := filepath.Join(dir, "catalog.json")
	if _, err := catalog.ReadJSON(path, cat); err != nil {
		cat = &catalog.Catalog{}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		errs = append(errs, err)
	} else {
		series, err := src.SyncSeries(s.ctx, env)
		if err != nil {
			errs = append(errs, fmt.Errorf("series: %w", err))
		}
		if err == nil || (len(series) > 0 && s.ctx.Err() == nil) {
			cat.Series = series
		}
		if s.ctx.Err() == nil {
			movies, err := src.SyncMovies(s.ctx, env)
			if err != nil {
				errs = append(errs, fmt.Errorf("movies: %w", err))
			}
			if err == nil || (len(movies) > 0 && s.ctx.Err() == nil) {
				cat.Movies = movies
			}
		}
	}

	cat.UpdatedAt = time.Now().UTC()
	if err := catalog.WriteJSON(path, cat); err != nil {
		errs = append(errs, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	ss.FinishedAt = &now
	ss.Series, ss.Movies = len(cat.Series), len(cat.Movies)
	ss.Phase = ""
	ss.State = Done
	for _, err := range errs {
		ss.State = Failed
		ss.Errors = append(ss.Errors, err.Error())
		s.logf("[%s] %v", src.Name(), err)
	}
	s.logf("[%s] %s: %d series, %d movies", src.Name(), ss.State, ss.Series, ss.Movies)
}

// merge combines the sources' saved catalogs into data.json and serves it.
func (s *Syncer) merge() error {
	var cats []*catalog.Catalog
	for _, src := range s.sources {
		c := &catalog.Catalog{}
		ok, err := catalog.ReadJSON(filepath.Join(s.dir, src.Name(), "catalog.json"), c)
		if err != nil {
			return fmt.Errorf("%s: %w", src.Name(), err)
		}
		if ok {
			cats = append(cats, c)
		}
	}
	merged := catalog.Merge(cats...)
	if merged.Series == nil {
		merged.Series = []*catalog.Title{}
	}
	if merged.Movies == nil {
		merged.Movies = []*catalog.Title{}
	}

	var raw bytes.Buffer
	enc := json.NewEncoder(&raw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(merged); err != nil {
		return err
	}
	if err := catalog.WriteFile(filepath.Join(s.dir, "data.json"), raw.Bytes()); err != nil {
		return err
	}

	var gz bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
	zw.Write(raw.Bytes())
	if err := zw.Close(); err != nil {
		return err
	}
	sum := sha256.Sum256(raw.Bytes())
	s.data.Store(&Data{Gzip: gz.Bytes(), ETag: `"` + hex.EncodeToString(sum[:8]) + `"`, Updated: merged.UpdatedAt})

	s.mu.Lock()
	s.status.Series, s.status.Movies = len(merged.Series), len(merged.Movies)
	s.mu.Unlock()
	log.Printf("merged %d source(s): %d series, %d movies (%d KB gzipped)", len(cats), len(merged.Series), len(merged.Movies), gz.Len()/1024)
	return nil
}

// logf adds an event to the status log. s.mu must be held.
func (s *Syncer) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
	s.status.Log = append(s.status.Log, time.Now().UTC().Format("15:04:05")+" "+msg)
	if n := len(s.status.Log) - maxLog; n > 0 {
		s.status.Log = s.status.Log[n:]
	}
}

// saveStatus writes the status to disk. s.mu must be held.
func (s *Syncer) saveStatus() {
	if err := catalog.WriteJSON(s.statusPath(), &s.status); err != nil {
		log.Printf("saving status: %v", err)
	}
}

// reporter feeds one source's progress into the status.
type reporter struct {
	s  *Syncer
	ss *SourceStatus
}

func (r *reporter) Phase(name string, total int) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.ss.Phase, r.ss.Done, r.ss.Total = name, 0, total
}

func (r *reporter) Done() {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.ss.Done++
}

func (r *reporter) Logf(format string, args ...any) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.logf("["+r.ss.Name+"] "+format, args...)
}
