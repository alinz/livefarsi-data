// Command scrape crawls the TV series catalog on diycraftsguide.com and saves
// every show, its poster, seasons, episodes and each episode's player URL.
//
// Each show is written to <dir>/series/<slug>.json as soon as it is done, so
// an interrupted run can simply be restarted and resumes where it stopped.
// At the end all shows are combined, in catalog order, into one JSON file.
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
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	dir := flag.String("dir", "output", "folder for the per-series JSON files")
	out := flag.String("o", "shows.json", "combined JSON file")
	workers := flag.Int("workers", 4, "shows processed in parallel")
	perShow := flag.Int("per-show", 3, "episode pages fetched in parallel per show")
	rps := flag.Float64("rps", 8, "maximum requests per second to the site, across all workers")
	retries := flag.Int("retries", 4, "retries per page on errors")
	limit := flag.Int("limit", 0, "only scrape the first N shows (0 = all)")
	refresh := flag.Bool("refresh", false, "re-scrape shows that already have a JSON file")
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
				if !*refresh && fileExists(path) {
					progress.FinishShow(w, "skipped")
					continue
				}
				progress.StartShow(w, s.Slug)
				err := scrapeShow(ctx, fetcher, progress, w, *perShow, s)
				if err == nil {
					err = writeJSON(path, s)
				}
				switch {
				case ctx.Err() != nil:
					progress.FinishShow(w, "interrupted")
				case err != nil:
					progress.Logf("\x1b[31m%s: %v (will retry on next run)\x1b[0m", s.Slug, err)
					progress.FinishShow(w, "failed")
				default:
					progress.FinishShow(w, "done")
				}
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
	for offset := 24; offset <= last; offset += 24 {
		page, err := f.Get(ctx, fmt.Sprintf("%s/tv-series/%d.html", baseURL, offset))
		if err != nil {
			return nil, err
		}
		more, _ := parseCatalogPage(page)
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

// scrapeShow reads the show page, then every episode page for its player.
func scrapeShow(ctx context.Context, f *Fetcher, progress *Progress, w, parallel int, s *Show) error {
	page, err := f.Get(ctx, s.URL)
	if err != nil {
		return err
	}
	parseShowPage(s, page)

	var todo []*Episode
	for _, ep := range s.episodes() {
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
			return ctx.Err()
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
		return ctx.Err()
	}
	if n := failed.Load(); n > 0 {
		return fmt.Errorf("%d episode pages failed, e.g. %v", n, first.Load())
	}
	return nil
}

// writeJSON writes v to path atomically, so a crash never leaves half a file.
func writeJSON(path string, v any) error {
	b, err := marshal(v)
	if err != nil {
		return err
	}
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
