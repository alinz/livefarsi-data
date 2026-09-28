package main

import (
	"html"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const baseURL = "https://www.diycraftsguide.com"

type Show struct {
	Slug         string    `json:"slug"`
	Title        string    `json:"title"`
	URL          string    `json:"url"`
	Poster       string    `json:"poster"`
	Description  string    `json:"description,omitempty"`
	EpisodeCount string    `json:"episode_count,omitempty"`
	Seasons      []*Season `json:"seasons"`
}

type Season struct {
	Name     string     `json:"name"`
	Episodes []*Episode `json:"episodes"`
}

type Episode struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Thumbnail string `json:"thumbnail,omitempty"`
	// Player is "iframe" for embeddable pages (ok.ru, YouTube, ...) or "video"
	// for a direct media file played with an HTML5 <video> element.
	Player   string `json:"player,omitempty"`
	Embed    string `json:"embed,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
}

func (s *Show) episodes() []*Episode {
	var eps []*Episode
	for _, se := range s.Seasons {
		eps = append(eps, se.Episodes...)
	}
	return eps
}

var (
	// Catalog page: one card per show, plus the "last page" pagination link.
	reCard = regexp.MustCompile(`(?s)<div class="latest-movie-img-container">.*?<img class="latest-movie-thumb" src="([^"]+)"[^>]*alt="([^"]*)".*?<a href="(` +
		regexp.QuoteMeta(baseURL) + `/watch/([^"/]+)\.html)".*?<span class="label label-primary">\s*(.*?)\s*</span>`)
	reLastPage = regexp.MustCompile(`href="` + regexp.QuoteMeta(baseURL) + `/tv-series/(\d+)\.html"[^>]*>»</a>`)

	// Show page: season headings and episode figures, in document order.
	reSeasonOrEpisode = regexp.MustCompile(`(?s)<div class="movie-heading overflow-hidden">\s*<span>(.*?)</span>|<figure class="figure">.*?<a href="([^"]+\?key=[^"]+)">.*?<img class="img-responsive" src="([^"]+)".*?<figcaption[^>]*>(.*?)</figcaption>`)
	reDescription     = regexp.MustCompile(`<meta property="og:description" content="([^"]*)"`)

	// Episode page: the player is an iframe, a video.js source list, or a <source> tag.
	reIframe      = regexp.MustCompile(`(?s)class="video-embed-container">\s*<iframe[^>]*?\ssrc="([^"]+)"`)
	reJSSources   = regexp.MustCompile(`(?s)sources['"]?\s*:\s*\[\s*\{(.*?)\}`)
	reJSSrc       = regexp.MustCompile(`['"]?src['"]?\s*:\s*['"]([^'"]+)['"]`)
	reJSType      = regexp.MustCompile(`['"]?type['"]?\s*:\s*['"]([^'"]+)['"]`)
	reVideoSource = regexp.MustCompile(`<source[^>]*?\ssrc="([^"]+)"[^>]*?\stype="([^"]+)"`)
	reYouTubeID   = regexp.MustCompile(`(?:youtu\.be/|youtube\.com/(?:watch\?v=|embed/|shorts/))([\w-]{11})`)
)

func text(s string) string {
	return strings.TrimSpace(html.UnescapeString(s))
}

func absURL(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}
	return u
}

// parseCatalogPage returns the shows on one catalog page and the offset of
// the last catalog page (0 if the page has no pagination).
func parseCatalogPage(page string) (shows []*Show, lastOffset int) {
	for _, m := range reCard.FindAllStringSubmatch(page, -1) {
		shows = append(shows, &Show{
			Slug:         m[4],
			Title:        text(m[2]),
			URL:          m[3],
			Poster:       m[1],
			EpisodeCount: text(m[5]),
		})
	}
	if m := reLastPage.FindStringSubmatch(page); m != nil {
		lastOffset, _ = strconv.Atoi(m[1])
	}
	return shows, lastOffset
}

// parseShowPage fills in description, seasons and episodes from the show page.
func parseShowPage(s *Show, page string) {
	if m := reDescription.FindStringSubmatch(page); m != nil {
		s.Description = text(m[1])
	}

	var cur *Season
	for _, m := range reSeasonOrEpisode.FindAllStringSubmatch(page, -1) {
		if m[2] == "" { // season heading
			cur = nil
			if name := text(m[1]); name != "" {
				cur = &Season{Name: name}
			}
			continue
		}
		if cur == nil {
			cur = &Season{Name: "Episodes"}
		}
		if len(cur.Episodes) == 0 {
			s.Seasons = append(s.Seasons, cur)
		}
		cur.Episodes = append(cur.Episodes, &Episode{
			Title:     text(m[4]),
			URL:       text(m[2]),
			Thumbnail: m[3],
		})
	}

	// The site lists newest first; store seasons and episodes in watch order.
	slices.Reverse(s.Seasons)
	for _, se := range s.Seasons {
		slices.Reverse(se.Episodes)
	}

	// A show without an episode list still has a player on its own page.
	if len(s.Seasons) == 0 {
		ep := &Episode{Title: s.Title, URL: s.URL}
		if ep.setPlayer(page); ep.Embed != "" {
			s.Seasons = []*Season{{Name: "Episodes", Episodes: []*Episode{ep}}}
		}
	}
}

// setPlayer finds the episode's player on its page and records it.
func (ep *Episode) setPlayer(page string) {
	if m := reIframe.FindStringSubmatch(page); m != nil {
		ep.Player, ep.Embed = "iframe", absURL(text(m[1]))
		return
	}
	var src, typ string
	if m := reJSSources.FindStringSubmatch(page); m != nil {
		if sm := reJSSrc.FindStringSubmatch(m[1]); sm != nil {
			src = sm[1]
			if tm := reJSType.FindStringSubmatch(m[1]); tm != nil {
				typ = tm[1]
			}
		}
	}
	if src == "" {
		if m := reVideoSource.FindStringSubmatch(page); m != nil {
			src, typ = m[1], m[2]
		}
	}
	if src == "" {
		return
	}
	src = absURL(text(src))
	if m := reYouTubeID.FindStringSubmatch(src); m != nil {
		ep.Player, ep.Embed = "iframe", "https://www.youtube.com/embed/"+m[1]
		return
	}
	ep.Player, ep.Embed, ep.MimeType = "video", src, text(typ)
}
