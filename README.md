# movies

A small Go server that syncs TV series and movies from several sites, merges them into one
catalog per section without duplicates, and serves a website to browse and watch them.

- **Farsi**: Iranian series and movies from LiveFarsi (diycraftsguide) and FarsiPlex.
- **Foreign**: international movies and series from FMovies (`ww.fmoviess.org`).

```sh
go run ./cmd/server -dir data -addr :8080    # then open http://localhost:8080
```

The page opens on a section chooser. Inside a section, **Sync** syncs only that section's
sources. On a title's page, **Sync this title** re-fetches just that title.

## Syncing

- **One at a time.** Each section runs at most one full sync at a time (a second request gets
  `409`). The two sections are separate sites, so they can sync at the same time. Each title
  syncs at most once at a time, too: a second request for the same title waits for the running
  one. A title sync you ask for is refused (`409`) while its section's full sync runs.
- **Resumable.** Progress is saved as it goes. The Farsi sources skip every title that is
  already up to date. FMovies saves a cursor after each listing page, so an interrupted sync
  continues from the next page.
- **Lazy details (Foreign).** FMovies has ~900 listing pages, so its full sync reads only the
  listings (title and poster). Opening a title reads its own page (and each season's page)
  first, then saves and returns it. Later full syncs read from the top and stop at the first
  page with nothing new; a **deep** sync reads every page.

The first Farsi sync takes about 20 minutes; after that a sync only fetches what changed and
takes seconds.

## API

Every route except `/` and `/config` takes `?section=farsi|foreign` (default `farsi`).

| | |
|---|---|
| `GET /` | the website (`web/index.html`) |
| `GET /api/sections` | the section names |
| `GET /rsync/start` | start the section's full sync in the background: `202` with the status, or `409` if one is already running. `?deep=1` re-checks every title instead of trusting the sites' listings |
| `GET /rsync/status` | the section's current or last sync: `state` (`IDLE`, `RUNNING`, `MERGING`, `DONE`, `FAILED`), start and finish times, per-source progress and errors, title syncs running now, and recent log lines |
| `GET /rsync/data` | the section's title list (id, title, poster, episode count) as JSON, gzip-compressed, with an `ETag` |
| `GET /api/title?kind=series\|movies&id=…` | one title with its seasons and players, as `{"title": …, "error": …}`. A title known only from a listing is synced first |
| `GET /rsync/title?kind=…&id=…` | sync one title on every source it is on, then return it the same way; `?deep=1` re-fetches everything. `409` while the section's full sync runs |
| `GET /config` | the routing config [baker](https://ella.to/baker) pings to proxy `farsi.ella.to/*` here (`internal/server/config.go`) |

## Storage

Everything is in one SQLite database, `<dir>/movies.db` ([ella.to/sqlite](https://github.com/ella-to/sqlite)).
The schema is in `internal/store/migrations/`, applied in order at start-up. To change it, add the
next numbered `.sql` file.

| table | holds |
|---|---|
| `items` | each source's working data: one JSON document per title (what a sync compares against), plus `state` documents such as FMovies' resume cursor |
| `titles` | each source's catalog, in display order: what the section merges and serves |
| `sync_status` | each section's last sync status, so it survives restarts |

On first start with an empty database, the JSON files of earlier versions
(`<dir>/<source>/{series,movies}/*.json`, `catalog.json`, `orphans.json`, `status.json`) are
imported, so upgrading doesn't need a full re-sync. The files are left in place and can be
deleted afterwards.

## Sources

Each site is a `source.Source` (`internal/source/source.go`) with a `Section`, `SyncSeries`,
`SyncMovies` and `SyncTitle`. A section's sources run in parallel, each with its own rate limit,
shared by its full and title syncs.

- **diycraftsguide** (LiveFarsi, `www.diycraftsguide.com`): the catalog pages list each
  series' episode count, so unchanged series cost no requests; a changed series re-reads its
  page and fetches only new episode pages. New movies are fetched once. 8 requests/s.
- **farsiplex** (`farsiplex.com`): its WordPress sitemaps give every movie, show and episode
  a last-modified date, so a sync reads the sitemaps and fetches only what is new or has a
  newer date. Players come from the site's player API, one per quality (480p/720p/1080p).
  5 requests/s.
- **fmoviess** (`ww.fmoviess.org`): listings at `/movies/` and `/tv-series/` (page n at
  `/movies//n/`), newest first. Each TV entry is one season ("WandaVision - Season 1"); seasons
  are grouped into one series. Title pages give the description and players. 4 requests/s.

To add a site, implement `source.Source` in `internal/source/<name>` and add it to the list in
`cmd/server/main.go`.

## Merging

`internal/catalog/merge.go` matches titles across a section's sources by a folded Latin title
(so "Bamdade Khomar" = "Bamdad Khomar", "Vahshi 2025" = "Vaahshi") or by the Persian title.
Matched series merge seasons and episodes by number, and each episode keeps every source's
players, so the page offers a **Source** switch (LiveFarsi, FarsiPlex 720p, …) when there is
more than one. The first source (diycraftsguide) wins for the title, poster and order; titles
only on farsiplex come after.

```sh
go test ./...
```
