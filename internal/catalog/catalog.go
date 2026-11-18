// Package catalog is the data model every source produces and the website
// consumes, plus the helpers to store it and merge several sources into one.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Catalog is everything the site shows.
type Catalog struct {
	UpdatedAt time.Time `json:"updated_at"`
	Series    []*Title  `json:"series"`
	Movies    []*Title  `json:"movies"`
}

// Title is a TV series (with Seasons) or a movie (with Players).
type Title struct {
	ID          string `json:"id"` // unique among the catalog's series, or its movies
	Title       string `json:"title"`
	TitleFa     string `json:"title_fa,omitempty"` // original Persian title, when a source has it
	Poster      string `json:"poster,omitempty"`
	Description string `json:"description,omitempty"`
	// Links are the title's pages on each source it came from.
	Links   []Link    `json:"links"`
	Seasons []*Season `json:"seasons,omitempty"`
	Players []*Player `json:"players,omitempty"`
	// Partial marks a title known only from a source's listing (title and
	// poster); its seasons and players are fetched when someone opens it.
	Partial bool `json:"partial,omitempty"`
}

type Link struct {
	Source string `json:"source"`
	URL    string `json:"url"`
}

type Season struct {
	Number   int        `json:"number,omitempty"` // 0 when the name has no number
	Name     string     `json:"name"`
	Episodes []*Episode `json:"episodes"`
}

type Episode struct {
	Number    int       `json:"number,omitempty"` // 0 when the title has no number
	Title     string    `json:"title"`
	Thumbnail string    `json:"thumbnail,omitempty"`
	Players   []*Player `json:"players"`
}

// Player is one way to watch an episode or movie. A title found on several
// sources has one player per source (and per quality, if a source has several).
type Player struct {
	Source string `json:"source"`
	Label  string `json:"label,omitempty"` // e.g. "720p"
	// Type is "iframe" for embeddable pages (ok.ru, YouTube, ...), "video"
	// for a media file played with an HTML5 <video> element, or "link" for a
	// page that can't be embedded and opens on its own site.
	Type string `json:"type"`
	URL  string `json:"url"`
	Mime string `json:"mime,omitempty"`
}

// Player types.
const (
	PlayerIframe = "iframe"
	PlayerVideo  = "video"
	PlayerLink   = "link"
)

// Playable reports how many episodes (or, for a movie, 1 or 0) can be played.
func (t *Title) Playable() int {
	if len(t.Seasons) == 0 {
		return min(len(t.Players), 1)
	}
	n := 0
	for _, se := range t.Seasons {
		for _, ep := range se.Episodes {
			if len(ep.Players) > 0 {
				n++
			}
		}
	}
	return n
}

// Marshal encodes v as indented JSON without escaping <, > and &.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	err := enc.Encode(v)
	return buf.Bytes(), err
}

// WriteFile writes b to path atomically, so a crash never leaves half a file.
func WriteFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WriteJSON stores v at path atomically.
func WriteJSON(path string, v any) error {
	b, err := Marshal(v)
	if err != nil {
		return err
	}
	return WriteFile(path, b)
}

// ReadJSON loads path into v. It reports false, with no error, if the file
// doesn't exist.
func ReadJSON(path string, v any) (bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(b, v)
}
