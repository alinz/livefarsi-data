// Package store keeps everything the server syncs in one SQLite database:
// each source's working data (items), each source's catalog (titles), and
// each section's sync status. The schema lives in migrations/, applied in
// order on Open.
package store

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"time"

	"ella.to/sqlite"

	"farsi.ella.to/internal/catalog"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Store struct {
	db *sqlite.Database
}

// Open opens (or creates) the database at path and applies any migrations
// it hasn't run yet.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sqlite.New(ctx, sqlite.WithFile(path), sqlite.WithPoolSize(8))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := sqlite.Migration(ctx, db, migrations, "migrations"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func step(stmt *sqlite.Stmt) error {
	_, err := stmt.Step()
	return err
}

// ---- items ----

// Item reads one item into v. It reports false, with no error, if there is none.
func (s *Store) Item(ctx context.Context, source, kind, slug string, v any) (bool, error) {
	var data string
	err := s.db.Exec(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `SELECT data FROM items WHERE source = ? AND kind = ? AND slug = ?`, source, kind, slug)
		if err != nil {
			return err
		}
		defer stmt.Reset()
		ok, err := stmt.Step()
		if ok {
			data = stmt.GetText("data")
		}
		return err
	})
	if err != nil || data == "" {
		return false, err
	}
	return true, json.Unmarshal([]byte(data), v)
}

// PutItem stores v as one item, replacing what was there.
func (s *Store) PutItem(ctx context.Context, source, kind, slug string, v any) error {
	b, err := catalog.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Write(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `
			INSERT INTO items (source, kind, slug, data, updated_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (source, kind, slug) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
			source, kind, slug, string(b), time.Now().Unix())
		if err != nil {
			return err
		}
		return step(stmt)
	})
}

// DeleteItem removes one item, if it exists.
func (s *Store) DeleteItem(ctx context.Context, source, kind, slug string) error {
	return s.db.Write(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `DELETE FROM items WHERE source = ? AND kind = ? AND slug = ?`, source, kind, slug)
		if err != nil {
			return err
		}
		return step(stmt)
	})
}

// Items calls fn with every item of a source and kind, in slug order.
func (s *Store) Items(ctx context.Context, source, kind string, fn func(slug string, data []byte) error) error {
	return s.db.Read(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `SELECT slug, data FROM items WHERE source = ? AND kind = ? ORDER BY slug`, source, kind)
		if err != nil {
			return err
		}
		defer stmt.Reset()
		for {
			ok, err := stmt.Step()
			if err != nil || !ok {
				return err
			}
			if err := fn(stmt.GetText("slug"), []byte(stmt.GetText("data"))); err != nil {
				return err
			}
		}
	})
}

// ---- titles ----

func titleKey(t *catalog.Title) string {
	if len(t.Links) > 0 {
		return t.Links[0].URL
	}
	return t.ID
}

// Titles returns a source's catalog of one kind, in display order.
func (s *Store) Titles(ctx context.Context, source, kind string) ([]*catalog.Title, error) {
	var out []*catalog.Title
	err := s.db.Read(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `SELECT data FROM titles WHERE source = ? AND kind = ? ORDER BY position`, source, kind)
		if err != nil {
			return err
		}
		defer stmt.Reset()
		for {
			ok, err := stmt.Step()
			if err != nil || !ok {
				return err
			}
			t := &catalog.Title{}
			if err := json.Unmarshal([]byte(stmt.GetText("data")), t); err != nil {
				return err
			}
			out = append(out, t)
		}
	})
	return out, err
}

// ReplaceTitles makes ts, in order, a source's whole catalog of one kind.
func (s *Store) ReplaceTitles(ctx context.Context, source, kind string, ts []*catalog.Title) error {
	return s.db.Write(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `DELETE FROM titles WHERE source = ? AND kind = ?`, source, kind)
		if err != nil {
			return err
		}
		if err := step(stmt); err != nil {
			return err
		}
		now := time.Now().Unix()
		for i, t := range ts {
			b, err := catalog.Marshal(t)
			if err != nil {
				return err
			}
			// A source may list the same page twice; the first one wins.
			stmt, err := conn.Prepare(ctx, `
				INSERT INTO titles (source, kind, url, position, data, updated_at) VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT (source, kind, url) DO NOTHING`,
				source, kind, titleKey(t), i, string(b), now)
			if err != nil {
				return err
			}
			if err := step(stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

// PutTitle replaces one title of a source's catalog (matched by its page
// URL), keeping its place; a new title goes last.
func (s *Store) PutTitle(ctx context.Context, source, kind string, t *catalog.Title) error {
	b, err := catalog.Marshal(t)
	if err != nil {
		return err
	}
	return s.db.Write(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `
			INSERT INTO titles (source, kind, url, position, data, updated_at)
			VALUES (?, ?, ?, (SELECT COALESCE(MAX(position), -1) + 1 FROM titles WHERE source = ? AND kind = ?), ?, ?)
			ON CONFLICT (source, kind, url) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
			source, kind, titleKey(t), source, kind, string(b), time.Now().Unix())
		if err != nil {
			return err
		}
		return step(stmt)
	})
}

// ---- sync status ----

// Status reads a section's saved sync status into v.
func (s *Store) Status(ctx context.Context, section string, v any) (bool, error) {
	var data string
	err := s.db.Exec(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `SELECT data FROM sync_status WHERE section = ?`, section)
		if err != nil {
			return err
		}
		defer stmt.Reset()
		ok, err := stmt.Step()
		if ok {
			data = stmt.GetText("data")
		}
		return err
	})
	if err != nil || data == "" {
		return false, err
	}
	return true, json.Unmarshal([]byte(data), v)
}

// PutStatus saves a section's sync status.
func (s *Store) PutStatus(ctx context.Context, section string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Write(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `
			INSERT INTO sync_status (section, data, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (section) DO UPDATE SET data = excluded.data, updated_at = excluded.updated_at`,
			section, string(b), time.Now().Unix())
		if err != nil {
			return err
		}
		return step(stmt)
	})
}

// Empty reports whether the database holds no items and no titles yet.
func (s *Store) Empty(ctx context.Context) (bool, error) {
	n := 0
	err := s.db.Exec(ctx, func(ctx context.Context, conn *sqlite.Conn) error {
		stmt, err := conn.Prepare(ctx, `SELECT (SELECT COUNT(*) FROM items) + (SELECT COUNT(*) FROM titles) AS n`)
		if err != nil {
			return err
		}
		defer stmt.Reset()
		if _, err := stmt.Step(); err != nil {
			return err
		}
		n = int(stmt.GetInt64("n"))
		return nil
	})
	return n == 0, err
}

// Bucket is one source's view of the items: what source.Env.Store is.
type Bucket struct {
	s      *Store
	source string
}

func (s *Store) Bucket(source string) *Bucket { return &Bucket{s: s, source: source} }

func (b *Bucket) Load(ctx context.Context, kind, slug string, v any) (bool, error) {
	return b.s.Item(ctx, b.source, kind, slug, v)
}

func (b *Bucket) Save(ctx context.Context, kind, slug string, v any) error {
	return b.s.PutItem(ctx, b.source, kind, slug, v)
}

func (b *Bucket) Delete(ctx context.Context, kind, slug string) error {
	return b.s.DeleteItem(ctx, b.source, kind, slug)
}

func (b *Bucket) Each(ctx context.Context, kind string, fn func(slug string, data []byte) error) error {
	return b.s.Items(ctx, b.source, kind, fn)
}
