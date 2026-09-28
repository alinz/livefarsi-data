# movies

Two small Go programs (standard library only):

1. **`cmd/scrape`** crawls https://www.diycraftsguide.com/serial.html (every catalog page),
   visits each show and each episode page, saves each series to `output/series/<slug>.json`
   as soon as it's done, and finally combines them (in catalog order) into `shows.json`.
2. **`cmd/site`** turns that JSON into a static website (plain HTML/CSS/JS, no frameworks).

```sh
go run ./cmd/scrape                         # → output/series/*.json + shows.json
go run ./cmd/site -i shows.json -o site     # then open site/index.html
```

### Scraper flags

| flag | default | |
|---|---|---|
| `-workers` | 4 | shows processed in parallel |
| `-per-show` | 3 | episode pages fetched in parallel within one show |
| `-rps` | 8 | hard cap on requests/second to the site, shared by all workers |
| `-retries` | 4 | retries per page (exponential back-off) |
| `-dir` | `output` | folder for per-series files |
| `-o` | `shows.json` | combined file |
| `-limit` | 0 | only the first N shows (for testing) |
| `-refresh` | false | re-scrape series that already have a file |

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
