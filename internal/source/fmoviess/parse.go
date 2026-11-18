package fmoviess

import (
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Item is what this source saves per listing entry. Movies are one item
// each; TV series are listed one season per page ("WandaVision - Season 1"),
// so each season is its own item and the catalog groups them by Show.
type Item struct {
	Slug   string `json:"slug"` // e.g. "wandavision-1-101990"
	URL    string `json:"url"`
	Title  string `json:"title"`
	Poster string `json:"poster,omitempty"`
	Badge  string `json:"badge,omitempty"` // the card's "HD" or "Eps 9"
	// Sort orders the catalog: lower is newer on the site.
	Sort int64 `json:"sort"`
	// Show and Season split a TV title: "WandaVision", 1.
	Show   string `json:"show,omitempty"`
	Season int    `json:"season,omitempty"`

	// Filled in by a title sync, from the title's own page.
	SyncedAt time.Time `json:"synced_at,omitzero"`
	Detail
}

// Detail is what a title's page adds to its listing card.
//
// The page has no player URLs: its script builds the player's address in the
// viewer's browser, with a token tied to the viewer's country and the time.
// So the catalog links each movie and episode to the title's page instead.
type Detail struct {
	MID         int        `json:"mid,omitempty"`  // the site's id for the title
	Mode        string     `json:"mode,omitempty"` // "movie" or "serie"
	Description string     `json:"description,omitempty"`
	Year        int        `json:"year,omitempty"`
	Duration    string     `json:"duration,omitempty"`
	Genres      []string   `json:"genres,omitempty"`
	Servers     []string   `json:"servers,omitempty"`  // "Server 1", ...
	Episodes    []*Episode `json:"episodes,omitempty"` // a movie has one ("Full HD")
}

type Episode struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// card is one title on a listing page.
type card struct {
	slug, url, title, poster, badge string
}

// The site's HTML is minified: attribute values may be unquoted.
const attrValue = `(?:"([^"]*)"|'([^']*)'|([^\s"'>]+))`

func attrRe(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\s` + regexp.QuoteMeta(name) + `=` + attrValue)
}

// attr returns the value of an attribute in a tag, or "".
func attr(re *regexp.Regexp, tag string) string {
	if m := re.FindStringSubmatch(tag); m != nil {
		return html.UnescapeString(m[1] + m[2] + m[3])
	}
	return ""
}

var (
	aHref    = attrRe("href")
	aDataSrc = attrRe("data-src")
	aSrc     = attrRe("src")
	aAlt     = attrRe("alt")
	aID      = attrRe("id")
	aTitle   = attrRe("title")
	aProp    = attrRe("property")
	aName    = attrRe("name")
	aContent = attrRe("content")
	aMID     = attrRe("data-mid")
	aMode    = attrRe("data-mode")

	// A card is a link to a title page wrapping its poster, title and badge.
	reLink     = regexp.MustCompile(`(?s)<a(\s[^>]*)>(.*?)</a>`)
	reFilm     = regexp.MustCompile(`/film/([^/?#"'\s>]+)/?$`)
	reImg      = regexp.MustCompile(`<img\s[^>]*>`)
	reHeading  = regexp.MustCompile(`(?s)<h[1-6][^>]*>(.*?)</h[1-6]>`)
	reEps      = regexp.MustCompile(`(?i)\bEps\s*(\d+)`)
	rePage     = regexp.MustCompile(`(?i)\shref=["']?(?:https?://[^/"'\s>]+)?/(?:movies|tv-series)/+(\d+)/?["'\s>]`)
	reSeasonOf = regexp.MustCompile(`(?i)^(.*?)\s*[-–:]\s*Season\s*(\d+)\s*$`)

	reTag   = regexp.MustCompile(`<[^>]+>`)
	reBlock = regexp.MustCompile(`(?i)</(?:p|div|li|h[1-6]|tr|dd|dt)>|<br\s*/?>`)
	reSpace = regexp.MustCompile(`[ \t\r\f\v]+`)
)

func text(s string) string {
	s = html.UnescapeString(reTag.ReplaceAllString(s, " "))
	return strings.TrimSpace(reSpace.ReplaceAllString(s, " "))
}

// parseListing returns the cards on a listing page and the highest page
// number its pagination links to (0 if none).
func parseListing(page string) (cards []card, last int) {
	seen := map[string]bool{}
	for _, m := range reLink.FindAllStringSubmatch(page, -1) {
		href, inner := attr(aHref, m[1]), m[2]
		f := reFilm.FindStringSubmatch(href)
		img := reImg.FindString(inner)
		// Text links to a title (breadcrumbs, "watch now") aren't cards.
		if f == nil || img == "" || seen[f[1]] {
			continue
		}
		c := card{slug: f[1], url: absURL(href)}
		for _, u := range []string{attr(aDataSrc, img), attr(aSrc, img)} {
			if u != "" && !strings.HasPrefix(u, "data:") {
				c.poster = absURL(u)
				break
			}
		}
		c.title = text(attr(aAlt, img))
		if h := reHeading.FindStringSubmatch(inner); h != nil && text(h[1]) != "" {
			c.title = text(h[1])
		}
		if c.title == "" {
			continue
		}
		rest := text(inner)
		if e := reEps.FindStringSubmatch(rest); e != nil {
			c.badge = "Eps " + e[1]
		} else if strings.Contains(rest, "HD") {
			c.badge = "HD"
		}
		seen[c.slug] = true
		cards = append(cards, c)
	}
	for _, m := range rePage.FindAllStringSubmatch(page, -1) {
		n, _ := strconv.Atoi(m[1])
		last = max(last, n)
	}
	return cards, last
}

// splitSeason splits "WandaVision - Season 1" into "WandaVision", 1. A title
// without a season is season 1 of itself.
func splitSeason(title string) (string, int) {
	if m := reSeasonOf.FindStringSubmatch(title); m != nil {
		n, _ := strconv.Atoi(m[2])
		return strings.TrimSpace(m[1]), n
	}
	return title, 1
}

func absURL(u string) string {
	switch {
	case strings.HasPrefix(u, "//"):
		return "https:" + u
	case strings.HasPrefix(u, "/"):
		return baseURL + u
	}
	return u
}

// ---- title page ----

var (
	reH1       = regexp.MustCompile(`(?s)<h1[^>]*>(.*?)</h1>`)
	reAfterH1  = regexp.MustCompile(`(?s)</h1>\s*<div[^>]*>(.*?)</div>`)
	reMeta     = regexp.MustCompile(`<meta\s[^>]*>`)
	reMIDTag   = regexp.MustCompile(`<[a-z]+\s[^>]*\sdata-mid=[^>]*>`)
	reButton   = regexp.MustCompile(`(?s)<button(\s[^>]*)>(.*?)</button>`)
	reServerID = regexp.MustCompile(`^srv-\d+$`)
	reEpID     = regexp.MustCompile(`^ep-(\d+)$`)
	reYear     = regexp.MustCompile(`(?i)\bRelease\s*:?\s*((?:19|20)\d\d)\b`)
	reDuration = regexp.MustCompile(`(?i)\bDuration\s*:?\s*((?:\d+\s*h\s*)?\d+\s*m(?:in)?)\b`)
	reGenre    = regexp.MustCompile(`(?i)\bGenres?\s*:\s*([^\n]+)`)
)

// parsePage reads a title page: its title, a poster (the page's share image),
// and the details.
func parsePage(page string) (title, poster string, d Detail) {
	if m := reH1.FindStringSubmatch(page); m != nil {
		title = text(m[1])
	}
	meta := map[string]string{}
	for _, tag := range reMeta.FindAllString(page, -1) {
		k := attr(aProp, tag)
		if k == "" {
			k = attr(aName, tag)
		}
		if _, ok := meta[k]; k != "" && !ok {
			meta[k] = attr(aContent, tag)
		}
	}
	poster = absURL(meta["og:image"])

	if m := reAfterH1.FindStringSubmatch(page); m != nil {
		d.Description = text(m[1])
	}
	if d.Description == "" {
		d.Description = strings.TrimSpace(cmpOr(meta["og:description"], meta["description"]))
	}

	if tag := reMIDTag.FindString(page); tag != "" {
		d.MID, _ = strconv.Atoi(attr(aMID, tag))
		d.Mode = attr(aMode, tag)
	}
	for _, m := range reButton.FindAllStringSubmatch(page, -1) {
		id := attr(aID, m[1])
		switch {
		case reServerID.MatchString(id):
			d.Servers = append(d.Servers, text(m[2]))
		case reEpID.MatchString(id):
			n, _ := strconv.Atoi(reEpID.FindStringSubmatch(id)[1])
			d.Episodes = append(d.Episodes, &Episode{Number: n, Title: cmpOr(attr(aTitle, m[1]), text(m[2]))})
		}
	}

	body := html.UnescapeString(reTag.ReplaceAllString(reBlock.ReplaceAllString(page, "\n"), " "))
	body = reSpace.ReplaceAllString(body, " ")
	if m := reYear.FindStringSubmatch(body); m != nil {
		d.Year, _ = strconv.Atoi(m[1])
	}
	if m := reDuration.FindStringSubmatch(body); m != nil {
		d.Duration = strings.Join(strings.Fields(m[1]), " ")
	}
	if m := reGenre.FindStringSubmatch(body); m != nil {
		for g := range strings.SplitSeq(m[1], ",") {
			if g = strings.TrimSpace(g); g != "" && len(g) < 30 {
				d.Genres = append(d.Genres, g)
			}
		}
	}
	return title, poster, d
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
