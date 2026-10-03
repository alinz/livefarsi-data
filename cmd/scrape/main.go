// Command scrape crawls the TV series catalog on diycraftsguide.com and saves
// every show, its poster, seasons, episodes and each episode's player URL.
//
// Each show is written to <dir>/series/<slug>.json as soon as it is done, and
// at the end all shows are combined, in catalog order, into one JSON file.
//
// Runs are incremental: the catalog pages list every show's episode count, so
// a show whose count matches its saved file is skipped without any request.
// Changed shows re-read only their show page, keep the player URLs already on
// disk, and fetch just the new episode pages. An interrupted run resumes too.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	dir := flag.String("dir", "output", "folder for the per-series JSON files")
	out := flag.String("o", "data.json", "combined JSON file")
	workers := flag.Int("workers", 4, "shows processed in parallel")
	perShow := flag.Int("per-show", 3, "episode pages fetched in parallel per show")
	rps := flag.Float64("rps", 8, "maximum requests per second to the site, across all workers")
	retries := flag.Int("retries", 4, "retries per page on errors")
	limit := flag.Int("limit", 0, "only scrape the first N shows (0 = all)")
	deep := flag.Bool("deep", false, "re-read every show page to find changes the episode count misses (still only fetches new episodes)")
	refresh := flag.Bool("refresh", false, "ignore saved files and re-scrape everything")
	flag.Parse()
	log.SetFlags(log.Ltime)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	seriesDir := filepath.Join(*dir, "series")
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		log.Fatal(err)
	}

	fetcher := NewFetcher(*rps, *retries)
	defer fetcher.Close()

	log.Printf("reading catalog (at most %.0f req/s)…", *rps)
	shows, err := listShows(ctx, fetcher)
	if err != nil {
		log.Fatal(err)
	}
	if *limit > 0 && len(shows) > *limit {
		shows = shows[:*limit]
	}
	log.Printf("found %d shows; %d workers × %d episode fetches each", len(shows), *workers, *perShow)

	progress := NewProgress(fetcher, *workers)
	progress.SetTotal(len(shows))
	go progress.Run()

	jobs := make(chan *Show)
	var wg sync.WaitGroup
	for w := range *workers {
		wg.Go(func() {
			for s := range jobs {
				path := filepath.Join(seriesDir, s.Slug+".json")
				status, added, err := syncShow(ctx, fetcher, progress, w, *perShow, s, path, *refresh, *deep)
				switch {
				case ctx.Err() != nil:
					status = "interrupted"
				case err != nil:
					progress.Logf("\x1b[31m%s: %v (will retry on next run)\x1b[0m", s.Slug, err)
					status = "failed"
				case status == "new":
					progress.Logf("\x1b[32m+ %s: new series, %d episodes\x1b[0m", s.Slug, added)
				case status == "updated" && added > 0:
					progress.Logf("\x1b[36m~ %s: %d new episode(s)\x1b[0m", s.Slug, added)
				case status == "updated":
					progress.Logf("\x1b[36m~ %s: updated\x1b[0m", s.Slug)
				}
				progress.FinishShow(w, status)
			}
		})
	}
feed:
	for _, s := range shows {
		select {
		case jobs <- s:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	progress.Stop()

	if ctx.Err() != nil {
		fmt.Fprintf(os.Stderr, "\nInterrupted. Finished shows are saved in %s; run the same command again to resume.\n", seriesDir)
		os.Exit(130)
	}

	log.Printf("sync: %s", progress.Summary())
	if *limit == 0 {
		if gone := staleFiles(seriesDir, shows); len(gone) > 0 {
			log.Printf("%d series are no longer in the catalog (kept on disk, left out of %s): %v", len(gone), *out, gone)
		}
	}

	n, missing, err := combine(*out, seriesDir, shows)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("wrote %s with %d shows", *out, n)
	if missing > 0 {
		log.Printf("%d shows failed and are not included; run again to retry them", missing)
		os.Exit(1)
	}
}

// listShows walks every catalog page and returns the shows in catalog order.
func listShows(ctx context.Context, f *Fetcher) ([]*Show, error) {
	first, err := f.Get(ctx, baseURL+"/serial.html")
	if err != nil {
		return nil, err
	}
	shows, last := parseCatalogPage(first)

	// Fetch the remaining pages concurrently (the fetcher still rate-limits),
	// then append them in page order.
	pages := make([][]*Show, last/24)
	errs := make([]error, len(pages))
	var wg sync.WaitGroup
	for i := range pages {
		wg.Go(func() {
			page, err := f.Get(ctx, fmt.Sprintf("%s/tv-series/%d.html", baseURL, (i+1)*24))
			pages[i], _ = parseCatalogPage(page)
			errs[i] = err
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	for _, more := range pages {
		shows = append(shows, more...)
	}

	seen := map[string]bool{}
	unique := shows[:0]
	for _, s := range shows {
		if !seen[s.Slug] {
			seen[s.Slug] = true
			unique = append(unique, s)
		}
	}
	return unique, nil
}

// syncShow brings one show's file up to date. status is "new", "updated" or
// "unchanged"; added is the number of episodes that weren't on disk before.
func syncShow(ctx context.Context, f *Fetcher, progress *Progress, w, parallel int, s *Show, path string, refresh, deep bool) (status string, added int, err error) {
	var old *Show
	if !refresh {
		if old, err = readShow(path); err != nil {
			progress.Logf("\x1b[33m%s: can't read saved file, re-scraping: %v\x1b[0m", s.Slug, err)
			old = nil
		}
	}
	if old != nil && !deep && old.EpisodeCount == s.EpisodeCount {
		return "unchanged", 0, nil
	}

	progress.StartShow(w, s.Slug)
	known := map[string]*Episode{}
	if old != nil {
		for _, ep := range old.episodes() {
			known[ep.URL] = ep
		}
	}
	if added, err = scrapeShow(ctx, f, progress, w, parallel, s, known); err != nil {
		return "", 0, err
	}

	b, err := marshal(s)
	if err != nil {
		return "", 0, err
	}
	if old != nil {
		if prev, err := os.ReadFile(path); err == nil && bytes.Equal(prev, b) {
			return "unchanged", 0, nil
		}
	}
	if err := writeFile(path, b); err != nil {
		return "", 0, err
	}
	if old == nil {
		return "new", added, nil
	}
	return "updated", added, nil
}

// readShow loads a saved show; it returns nil, nil if there is none.
func readShow(path string) (*Show, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Show
	return &s, json.Unmarshal(b, &s)
}

// scrapeShow reads the show page, then the episode pages for their players.
// Episodes found in known (from the previous run) keep their saved player, so
// only new episodes, or ones that had no player last time, are fetched.
func scrapeShow(ctx context.Context, f *Fetcher, progress *Progress, w, parallel int, s *Show, known map[string]*Episode) (added int, err error) {
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
			ep.Player, ep.Embed, ep.MimeType = prev.Player, prev.Embed, prev.MimeType
		}
		if ep.Embed == "" {
			todo = append(todo, ep)
		}
	}
	progress.SetEpisodes(w, len(todo))

	var (
		wg     sync.WaitGroup
		sem    = make(chan struct{}, parallel)
		failed atomic.Int32
		first  atomic.Value
	)
	for _, ep := range todo {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return 0, ctx.Err()
		}
		wg.Go(func() {
			defer func() { <-sem }()
			page, err := f.Get(ctx, ep.URL)
			switch {
			case err == nil:
				ep.setPlayer(page)
				if ep.Embed == "" {
					progress.Logf("\x1b[33mno player found on %s\x1b[0m", ep.URL)
				}
			case errors.Is(err, errNotFound):
				progress.Logf("\x1b[33m%v\x1b[0m", err)
			default:
				failed.Add(1)
				first.CompareAndSwap(nil, err)
			}
			progress.EpisodeDone(w, ep.Embed != "")
		})
	}
	wg.Wait()

	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if n := failed.Load(); n > 0 {
		return 0, fmt.Errorf("%d episode pages failed, e.g. %v", n, first.Load())
	}
	return added, nil
}

// writeFile writes b to path atomically, so a crash never leaves half a file.
func writeFile(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return buf.Bytes(), err
}

// combine streams the per-show files, in catalog order, into one JSON file
// without holding the whole catalog in memory.
func combine(out, seriesDir string, shows []*Show) (written, missing int, err error) {
	tmp := out + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, 0, err
	}
	defer os.Remove(tmp)

	header, _ := marshal(map[string]string{
		"source":     baseURL + "/serial.html",
		"scraped_at": time.Now().UTC().Format(time.RFC3339),
	})
	// Reopen the header object to append the shows array to it.
	header = bytes.TrimRight(header, "}\n")
	fmt.Fprintf(f, "%s,\n  \"shows\": [", header)

	var buf bytes.Buffer
	for _, s := range shows {
		b, err := os.ReadFile(filepath.Join(seriesDir, s.Slug+".json"))
		if errors.Is(err, fs.ErrNotExist) {
			missing++
			continue
		}
		if err != nil {
			f.Close()
			return 0, 0, err
		}
		buf.Reset()
		if err := json.Indent(&buf, bytes.TrimSpace(b), "    ", "  "); err != nil {
			f.Close()
			return 0, 0, fmt.Errorf("%s.json: %w", s.Slug, err)
		}
		if written > 0 {
			f.WriteString(",")
		}
		f.WriteString("\n    ")
		f.Write(buf.Bytes())
		written++
	}
	fmt.Fprint(f, "\n  ]\n}\n")
	if err := f.Close(); err != nil {
		return 0, 0, err
	}
	return written, missing, os.Rename(tmp, out)
}

// staleFiles lists saved series that are no longer in the catalog.
func staleFiles(seriesDir string, shows []*Show) []string {
	listed := map[string]bool{}
	for _, s := range shows {
		listed[s.Slug+".json"] = true
	}
	entries, _ := os.ReadDir(seriesDir)
	var gone []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" && !listed[e.Name()] {
			gone = append(gone, strings.TrimSuffix(e.Name(), ".json"))
		}
	}
	return gone
}
