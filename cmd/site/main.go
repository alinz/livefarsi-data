// Command site turns the JSON catalog produced by cmd/scrape into a static
// website: an index page listing every show (with search) and one page per
// show with its episodes and an embedded player.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"html/template"
	"log"
	"os"
	"path/filepath"
	"strings"
)

//go:embed templates/*.html assets/*
var files embed.FS

type Catalog struct {
	Source string  `json:"source"`
	Shows  []*Show `json:"shows"`
}

type Show struct {
	Slug         string    `json:"slug"`
	Title        string    `json:"title"`
	URL          string    `json:"url"`
	Poster       string    `json:"poster"`
	Description  string    `json:"description"`
	EpisodeCount string    `json:"episode_count"`
	Seasons      []*Season `json:"seasons"`
}

type Season struct {
	Name     string     `json:"name"`
	Episodes []*Episode `json:"episodes"`
}

type Episode struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Thumbnail string `json:"thumbnail"`
	Player    string `json:"player"`
	Embed     string `json:"embed"`
	MimeType  string `json:"mime_type"`
}

// Episodes returns the number of playable episodes across all seasons.
func (s *Show) Episodes() int {
	n := 0
	for _, se := range s.Seasons {
		for _, ep := range se.Episodes {
			if ep.Embed != "" {
				n++
			}
		}
	}
	return n
}

// SearchText is what the index page's search box matches against.
func (s *Show) SearchText() string {
	return strings.ToLower(s.Title + " " + s.Slug + " " + s.Description)
}

func main() {
	in := flag.String("i", "data.json", "input JSON file from cmd/scrape")
	out := flag.String("o", "site", "output directory")
	flag.Parse()

	data, err := os.ReadFile(*in)
	if err != nil {
		log.Fatal(err)
	}
	var cat Catalog
	if err := json.Unmarshal(data, &cat); err != nil {
		log.Fatal(err)
	}

	tmpl := template.Must(template.New("").Funcs(template.FuncMap{
		"add": func(a, b int) int { return a + b },
	}).ParseFS(files, "templates/*.html"))

	if err := os.MkdirAll(filepath.Join(*out, "shows"), 0o755); err != nil {
		log.Fatal(err)
	}
	for _, name := range []string{"style.css", "index.js", "show.js"} {
		b, err := files.ReadFile("assets/" + name)
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(*out, name), b, 0o644); err != nil {
			log.Fatal(err)
		}
	}

	render := func(path, name string, data any) {
		f, err := os.Create(path)
		if err != nil {
			log.Fatal(err)
		}
		if err := tmpl.ExecuteTemplate(f, name, data); err != nil {
			log.Fatalf("%s: %v", path, err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	}

	render(filepath.Join(*out, "index.html"), "index.html", cat)
	for _, s := range cat.Shows {
		render(filepath.Join(*out, "shows", s.Slug+".html"), "show.html", s)
	}
	log.Printf("wrote %s: index + %d show pages", *out, len(cat.Shows))
}
