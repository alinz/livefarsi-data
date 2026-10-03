# movies

Two small Go programs (standard library only):

1. **`cmd/scrape`** crawls https://www.diycraftsguide.com/serial.html (every catalog page),
   visits each show and each episode page, saves each series to `output/series/<slug>.json`
   as soon as it's done, and finally combines them (in catalog order) into `data.json`.
2. **`cmd/site`** turns that JSON into a static website (plain HTML/CSS/JS, no frameworks).

```sh
go run ./cmd/scrape                         # → output/series/*.json + data.json
go run ./cmd/site -o site                   # then open site/index.html
```

## Live data on GitHub

This repo is [alinz/livefarsi-data](https://github.com/alinz/livefarsi-data).
`.github/workflows/scrape.yml` runs every 2 hours (or by hand from the Actions tab, with an
optional *deep* checkbox). It runs the scraper incrementally against the committed
`output/series/*.json`, rewrites `data.json`, and commits both back, but only when something other
than `scraped_at` changed. If some series fail, everything else is still committed and the run is
marked failed. The failed series are retried on the next run.

`index.html` is a standalone copy of the generated site (same markup and CSS) that fetches
`https://raw.githubusercontent.com/alinz/livefarsi-data/main/data.json` at load time. Open it
from disk or host it anywhere (e.g. GitHub Pages) and it always shows the latest data.
Routes: `#/` (list, `?q=` search), `#/<slug>` (show), `#/<slug>/s1e2` (episode).

### Scraper flags

| flag | default | |
|---|---|---|
| `-workers` | 4 | shows processed in parallel |
| `-per-show` | 3 | episode pages fetched in parallel within one show |
| `-rps` | 8 | hard cap on requests/second to the site, shared by all workers |
| `-retries` | 4 | retries per page (exponential back-off) |
| `-dir` | `output` | folder for per-series files |
| `-o` | `data.json` | combined file |
| `-limit` | 0 | only the first N shows (for testing) |
| `-deep` | false | re-read every series page (≈40s) to catch changes the episode count misses |
| `-refresh` | false | ignore saved files and re-scrape everything |

### Incremental sync

Only the first run is a full crawl (~15 min). Every later run:

1. reads the 13 catalog pages (~2s), which show each series' episode count;
2. **skips** a series whose count matches its saved file, with no requests at all;
3. for a **new** series, scrapes it fully; for a series whose **count changed**, re-reads its
   page (1 request), keeps the player URLs already saved, and fetches only the new episode pages;
4. rewrites `data.json`.

So a run with nothing new takes ~3s, and a few new episodes add a second or two. The log lists
what changed (`+ koori: new series, 12 episodes`, `~ vaahshi: 2 new episode(s)`) and ends with a
summary line. Series that drop out of the catalog stay on disk but are left out of `data.json`.

The count check misses edits that don't change the count (e.g. an episode swapped for another),
so run with `-deep` now and then; it still only fetches episode pages it hasn't seen.

Example cron (every 30 minutes, plus a deep check nightly):

```cron
*/30 * * * * cd ~/Documents/ali/projects/movies && ./bin/scrape >> sync.log 2>&1 && ./bin/site
0 4 * * *    cd ~/Documents/ali/projects/movies && ./bin/scrape -deep >> sync.log 2>&1 && ./bin/site
```

(build the binaries with `go build -o bin/ ./cmd/...`)

Being polite to the server: all workers share one rate limiter, so `-workers`/`-per-show` only
control how many requests may be *in flight*; throughput never exceeds `-rps`. On HTTP 429 or
5xx every worker pauses (honouring `Retry-After`), not just the one that got the error.

Resuming: Ctrl+C stops cleanly. Finished series stay on disk (files are written atomically) and
are skipped on the next run, so just run the same command again. A series with failed episode
pages isn't saved, so it is retried next time.

In a terminal it shows live progress bars for shows and episodes, request rate, busy workers,
ETA, and one line per worker with its current series; when output is redirected it prints a
summary line every 5 seconds instead.

## JSON shape

```json
{
  "source": "...", "scraped_at": "...",
  "shows": [{
    "slug": "koori", "title": "Koori", "url": "...", "poster": "...jpg",
    "description": "...", "episode_count": "12 EPISODES",
    "seasons": [{
      "name": "Season 1",
      "episodes": [{
        "title": "E01", "url": "...?key=...", "thumbnail": "...jpg",
        "player": "iframe", "embed": "https://ok.ru/videoembed/...",
        "mime_type": ""
      }]
    }]
  }]
}
```

`player` is `iframe` (embed page, e.g. ok.ru) or `video` (direct media file, played with `<video>`;
`mime_type` says which format).

## Site

- `index.html`: poster grid of every series with a search box (matches title, slug and the
  Persian description; press `/` to focus, `Esc` to clear; the query is kept in `?q=`).
- `shows/<slug>.html`: poster, description, a player, and every season/episode. Clicking an
  episode loads it into the player; Previous/Next buttons, a `#s1e3` link to each episode, and
  it remembers the last episode you watched per show.
