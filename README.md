# movies

A small Go server (standard library only) that syncs Iranian TV series and movies from two
sites, merges them into one catalog without duplicates, and serves a website to browse and
watch them.

```sh
go run ./cmd/server -dir data -addr :8080    # then open http://localhost:8080
```

Click **Sync** in the page (or `GET /rsync/start`) to fetch the catalog. The first sync takes
about 20 minutes; after that, a sync only fetches what changed and takes seconds.

## API

| | |
|---|---|
| `GET /` | the website (`web/index.html`) |
| `GET /rsync/start` | start a sync in the background: `202` with the status, or `409` if one is already running. `?deep=1` re-checks every title instead of trusting the sites' listings |
| `GET /rsync/status` | the current or last sync: `state` (`IDLE`, `RUNNING`, `MERGING`, `DONE`, `FAILED`), start and finish times, per-source progress and errors, and recent log lines |
| `GET /rsync/data` | the merged catalog as JSON, gzip-compressed (~3 MB → a few hundred KB), with an `ETag` |
| `GET /config` | the routing config [baker](https://ella.to/baker) pings to proxy `farsi.ella.to/*` here (`internal/server/config.go`) |

## Data directory

Everything lives in the folder passed with `-dir`:

```
data/
  diycraftsguide/series/<slug>.json   each source's working files: what a sync compares
  diycraftsguide/movies/<slug>.json   against to fetch only new or changed titles
  diycraftsguide/catalog.json         the source's last good catalog
  farsiplex/...                       same layout
  data.json                           all sources merged, duplicates removed (served at /rsync/data)
  status.json                         the last sync's status, so it survives restarts
```

Files are written atomically, so stopping the server mid-sync is safe: the next sync picks up
where it left off.

## Sources

Each site is a `source.Source` (`internal/source/source.go`) with `SyncSeries` and
`SyncMovies`; both sources run in parallel, each with its own rate limit.

- **diycraftsguide** (LiveFarsi, `www.diycraftsguide.com`): the catalog pages list each
  series' episode count, so unchanged series cost no requests; a changed series re-reads its
  page and fetches only new episode pages. New movies are fetched once. 8 requests/s.
- **farsiplex** (`farsiplex.com`): its WordPress sitemaps give every movie, show and episode
  a last-modified date, so a sync reads the sitemaps and fetches only what is new or has a
  newer date. Players come from the site's player API, one per quality (480p/720p/1080p).
  5 requests/s.

To add a site, implement `source.Source` in `internal/source/<name>` and add it to the list in
`cmd/server/main.go`.

## Merging

`internal/catalog/merge.go` matches titles across sources by a folded Latin title (so
"Bamdade Khomar" = "Bamdad Khomar", "Vahshi 2025" = "Vaahshi") or by the Persian title.
Matched series merge seasons and episodes by number, and each episode keeps every source's
players, so the page offers a **Source** switch (LiveFarsi, FarsiPlex 720p, …) when there is
more than one. The first source (diycraftsguide) wins for the title, poster and order; titles
only on farsiplex come after.

```sh
go test ./...
```
