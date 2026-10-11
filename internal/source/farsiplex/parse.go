package farsiplex

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Show and Movie are what this source saves per title. LastMod is the date in
// the site's sitemap when the title (or episode) was last synced; a different
// date in the sitemap means it changed.
type Show struct {
	Slug    string    `json:"slug"`
	URL     string    `json:"url"`
	LastMod string    `json:"lastmod"`
	Info              // from the show page
	Seasons []*Season `json:"seasons"`
}

type Season struct {
	Number   int        `json:"number"`
	Episodes []*Episode `json:"episodes"`
}

type Episode struct {
	URL       string    `json:"url"`
	LastMod   string    `json:"lastmod"`
	PostID    int       `json:"post_id,omitempty"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Thumbnail string    `json:"thumbnail,omitempty"`
	Players   []*Player `json:"players,omitempty"`
}

type Movie struct {
	Slug    string    `json:"slug"`
	URL     string    `json:"url"`
	LastMod string    `json:"lastmod"`
	PostID  int       `json:"post_id,omitempty"`
	Info              // from the movie page
	Players []*Player `json:"players,omitempty"`
}

// Info is what a show or movie page says about the title.
type Info struct {
	Title       string `json:"title"`
	TitleFa     string `json:"title_fa,omitempty"`
	Poster      string `json:"poster,omitempty"`
	Description string `json:"description,omitempty"`
}

// Player is one quality of a title, played in the site's own embeddable
// jwplayer page (which signs the video URL each time it loads).
type Player struct {
	Label string `json:"label,omitempty"`
	URL   string `json:"url"`
}

var (
	rePostID   = regexp.MustCompile(`<body[^>]*\bpostid-(\d+)`)
	reTitle    = regexp.MustCompile(`(?s)<div class="sheader">.*?<h1>(.*?)</h1>`)
	rePoster   = regexp.MustCompile(`(?s)<div class="sheader">\s*<div class="poster">\s*<img[^>]*?\ssrc="([^"]+)"`)
	reSize     = regexp.MustCompile(`-\d+x\d+(\.\w+)$`)
	reSynopsis = regexp.MustCompile(`(?s)<h2>Synopsis</h2>\s*<div[^>]*class="wp-content">(.*?)(?:<ul class="wp-tags">|<div id='dt_galery'|</div>)`)
	reOriginal = regexp.MustCompile(`<b class="variante">Original title</b>\s*<span class="valor">([^<]*)</span>`)
	reBreak    = regexp.MustCompile(`(?i)</p>|<br\s*/?>|<hr\s*/?>`)
	reTag      = regexp.MustCompile(`<[^>]+>`)
	reBlank    = regexp.MustCompile(`\s*\n\s*`)

	reSeasonBlock = regexp.MustCompile(`<div class='se-c'>`)
	reSeasonNum   = regexp.MustCompile(`<span class='se-t[^']*'>(\d+)</span>`)
	reEpisode     = regexp.MustCompile(`(?s)<li class='mark-\d+'>.*?<img src='([^']*)'.*?<div class='numerando'>\s*(\d+)\s*-\s*(\d+)\s*</div>.*?<a href='([^']+)'>(.*?)</a>`)

	reQuality = regexp.MustCompile(`\.(\d{3,4})p?\.`)
)

func text(s string) string {
	return strings.TrimSpace(html.UnescapeString(reTag.ReplaceAllString(s, "")))
}

func postID(page string) int {
	if m := rePostID.FindStringSubmatch(page); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// parseInfo reads the title, poster, synopsis and original title from a show
// or movie page.
func parseInfo(page string) Info {
	var in Info
	if m := reTitle.FindStringSubmatch(page); m != nil {
		in.Title = text(m[1])
	}
	if m := rePoster.FindStringSubmatch(page); m != nil {
		// Thumbnails are named like kanape-1-220x300.jpg; drop the size for the full image.
		in.Poster = reSize.ReplaceAllString(m[1], "$1")
	}
	if m := reSynopsis.FindStringSubmatch(page); m != nil {
		d := reBreak.ReplaceAllString(m[1], "\n")
		in.Description = strings.TrimSpace(reBlank.ReplaceAllString(text(d), "\n"))
	}
	if m := reOriginal.FindStringSubmatch(page); m != nil {
		in.TitleFa = text(m[1])
	}
	return in
}

// parseSeasons reads the season and episode list on a show page.
func parseSeasons(page string) []*Season {
	var seasons []*Season
	blocks := reSeasonBlock.Split(page, -1)
	for _, b := range blocks[1:] {
		se := &Season{}
		if m := reSeasonNum.FindStringSubmatch(b); m != nil {
			se.Number, _ = strconv.Atoi(m[1])
		}
		for _, m := range reEpisode.FindAllStringSubmatch(b, -1) {
			n, _ := strconv.Atoi(m[3])
			se.Episodes = append(se.Episodes, &Episode{
				URL:       m[4],
				Number:    n,
				Title:     text(m[5]),
				Thumbnail: m[1],
			})
		}
		if len(se.Episodes) > 0 {
			seasons = append(seasons, se)
		}
	}
	return seasons
}

// quality reads "720p" from a player URL whose video file is named like
// Kanape.720.new.mp4.
func quality(embed string) string {
	src := embed
	if u, err := url.Parse(embed); err == nil && u.Query().Get("source") != "" {
		src = u.Query().Get("source")
	}
	if m := reQuality.FindStringSubmatch(src); m != nil {
		return m[1] + "p"
	}
	return ""
}

// baseSlug drops the random suffix WordPress adds to farsiplex slugs
// ("sarzamine-madari-17222071" -> "sarzamine-madari").
func baseSlug(slug string) string {
	if i := strings.LastIndexByte(slug, '-'); i > 0 && len(slug)-i == 9 {
		if _, err := strconv.ParseUint(slug[i+1:], 16, 64); err == nil {
			return slug[:i]
		}
	}
	return slug
}
