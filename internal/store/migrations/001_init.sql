-- items is each source's own working data, one JSON document per title (and
-- a few "state" documents such as a listing sync's resume cursor). A later
-- sync compares against it to fetch only what is new or changed.
CREATE TABLE items (
    source     TEXT    NOT NULL,
    kind       TEXT    NOT NULL, -- "series", "movies" or "state"
    slug       TEXT    NOT NULL,
    data       TEXT    NOT NULL, -- JSON
    updated_at INTEGER NOT NULL, -- unix seconds
    PRIMARY KEY (source, kind, slug)
) WITHOUT ROWID;

-- titles is each source's catalog: what it last produced for the site, in
-- display order. Sections merge their sources' titles into what is served.
CREATE TABLE titles (
    source     TEXT    NOT NULL,
    kind       TEXT    NOT NULL, -- "series" or "movies"
    url        TEXT    NOT NULL, -- the title's page on the source (Links[0])
    position   INTEGER NOT NULL,
    data       TEXT    NOT NULL, -- JSON catalog.Title
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (source, kind, url)
);

CREATE INDEX titles_order ON titles (source, kind, position);

-- sync_status is each section's last sync status, so it survives restarts.
CREATE TABLE sync_status (
    section    TEXT    PRIMARY KEY,
    data       TEXT    NOT NULL, -- JSON
    updated_at INTEGER NOT NULL
);
