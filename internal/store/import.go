package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"farsi.ella.to/internal/catalog"
)

// ImportFiles loads what earlier versions of the server kept as JSON files
// in dir (before the database), so an upgrade doesn't need a full re-sync:
//
//	<dir>/<source>/series/<slug>.json  -> items (source, "series", slug)
//	<dir>/<source>/movies/<slug>.json  -> items (source, "movies", slug)
//	<dir>/<source>/orphans.json        -> items (source, "state", "orphans")
//	<dir>/<source>/catalog.json        -> titles
//
// It reports how many items it imported. The files are left in place.
func (s *Store) ImportFiles(ctx context.Context, dir string, sources []string) (int, error) {
	n := 0
	for _, src := range sources {
		for _, kind := range []string{"series", "movies"} {
			files, err := filepath.Glob(filepath.Join(dir, src, kind, "*.json"))
			if err != nil {
				return n, err
			}
			for _, f := range files {
				b, err := os.ReadFile(f)
				if err != nil {
					return n, err
				}
				var v json.RawMessage
				if err := json.Unmarshal(b, &v); err != nil {
					return n, fmt.Errorf("%s: %w", f, err)
				}
				if err := s.PutItem(ctx, src, kind, strings.TrimSuffix(filepath.Base(f), ".json"), v); err != nil {
					return n, err
				}
				n++
			}
		}

		if b, err := os.ReadFile(filepath.Join(dir, src, "orphans.json")); err == nil {
			var v json.RawMessage
			if json.Unmarshal(b, &v) == nil {
				if err := s.PutItem(ctx, src, "state", "orphans", v); err != nil {
					return n, err
				}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return n, err
		}

		cat := &catalog.Catalog{}
		ok, err := catalog.ReadJSON(filepath.Join(dir, src, "catalog.json"), cat)
		if err != nil {
			return n, err
		}
		if ok {
			if err := s.ReplaceTitles(ctx, src, "series", cat.Series); err != nil {
				return n, err
			}
			if err := s.ReplaceTitles(ctx, src, "movies", cat.Movies); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}
