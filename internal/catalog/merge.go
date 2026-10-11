package catalog

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Merge combines the catalogs of several sources into one. A title that
// appears on more than one source becomes a single title: seasons are matched
// by number, episodes by number within a season, and every source's players
// are kept, so the viewer can switch between them.
//
// Titles match when their Latin titles share a Key or their Persian titles
// share a KeyFa. Titles within one source are never merged with each other.
// The first catalog wins for display fields (title, poster, ...) and its
// order comes first; titles found only in later catalogs are appended.
func Merge(cats ...*Catalog) *Catalog {
	out := &Catalog{}
	var series, movies [][]*Title
	for _, c := range cats {
		if c == nil {
			continue
		}
		series = append(series, c.Series)
		movies = append(movies, c.Movies)
		if c.UpdatedAt.After(out.UpdatedAt) {
			out.UpdatedAt = c.UpdatedAt
		}
	}
	out.Series = mergeTitles(series)
	out.Movies = mergeTitles(movies)
	return out
}

func mergeTitles(sources [][]*Title) []*Title {
	var (
		out   []*Title
		byKey = map[string]*Title{}
		ids   = map[string]bool{}
	)
	for _, titles := range sources {
		// Look matches up only among earlier sources, and register this
		// source's keys after the loop, so a source never merges with itself.
		matched := map[*Title]bool{}
		var added []*Title
		for _, t := range titles {
			t = clone(t)
			dst := byKey["en:"+Key(t.Title)]
			if fa := KeyFa(t.TitleFa); dst == nil && fa != "" {
				dst = byKey["fa:"+fa]
			}
			if dst != nil && !matched[dst] {
				matched[dst] = true
				mergeInto(dst, t)
				continue
			}
			t.ID = uniqueID(ids, t.ID, t.Title)
			out = append(out, t)
			added = append(added, t)
		}
		for _, t := range added {
			for _, k := range []string{"en:" + Key(t.Title), "fa:" + KeyFa(t.TitleFa)} {
				if k != "en:" && k != "fa:" && byKey[k] == nil {
					byKey[k] = t
				}
			}
		}
	}
	return out
}

func mergeInto(dst, src *Title) {
	dst.TitleFa = cmpOr(dst.TitleFa, src.TitleFa)
	dst.Poster = cmpOr(dst.Poster, src.Poster)
	dst.Description = cmpOr(dst.Description, src.Description)
	dst.Links = append(dst.Links, src.Links...)
	dst.Players = append(dst.Players, src.Players...)

	for _, sse := range src.Seasons {
		var dse *Season
		if sse.Number > 0 {
			for _, se := range dst.Seasons {
				if se.Number == sse.Number {
					dse = se
					break
				}
			}
		}
		if dse == nil {
			dst.Seasons = append(dst.Seasons, sse)
			continue
		}
		for _, sep := range sse.Episodes {
			var dep *Episode
			if sep.Number > 0 {
				for _, ep := range dse.Episodes {
					if ep.Number == sep.Number {
						dep = ep
						break
					}
				}
			}
			if dep == nil {
				dse.Episodes = append(dse.Episodes, sep)
				continue
			}
			dep.Thumbnail = cmpOr(dep.Thumbnail, sep.Thumbnail)
			dep.Players = append(dep.Players, sep.Players...)
		}
		// Episodes added from src belong in number order; unnumbered ones
		// keep their place at the end.
		slices.SortStableFunc(dse.Episodes, func(a, b *Episode) int { return order(a.Number, b.Number) })
	}
	slices.SortStableFunc(dst.Seasons, func(a, b *Season) int { return order(a.Number, b.Number) })
}

// order sorts numbers ascending with 0 (unknown) last.
func order(a, b int) int {
	switch {
	case a == b:
		return 0
	case a == 0:
		return 1
	case b == 0:
		return -1
	}
	return a - b
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// clone copies t deeply enough that merging never changes the input.
func clone(t *Title) *Title {
	c := *t
	c.Links = slices.Clone(t.Links)
	c.Players = slices.Clone(t.Players)
	c.Seasons = nil
	for _, se := range t.Seasons {
		s := *se
		s.Episodes = nil
		for _, ep := range se.Episodes {
			e := *ep
			e.Players = slices.Clone(ep.Players)
			s.Episodes = append(s.Episodes, &e)
		}
		c.Seasons = append(c.Seasons, &s)
	}
	return &c
}

func uniqueID(ids map[string]bool, id, title string) string {
	if id == "" {
		id = Slug(title)
	}
	base := id
	for n := 2; ids[id]; n++ {
		id = base + "-" + strconv.Itoa(n)
	}
	ids[id] = true
	return id
}

var (
	reYear    = regexp.MustCompile(`\b(19|20)\d\d\b`)
	reNonWord = regexp.MustCompile(`[^a-z0-9]+`)
	reSlug    = regexp.MustCompile(`[^a-z0-9]+`)
)

// Key folds a Latin title so that common transliteration differences of the
// same Persian name match: "Bamdade Khomar" and "Bamdad Khomar", "Vahshi
// 2025" and "Vaahshi", "Hezartoo" and "Hezartou" all give the same key.
func Key(title string) string {
	t := strings.ToLower(title)
	t = reYear.ReplaceAllString(t, " ")
	var b strings.Builder
	for _, w := range strings.Fields(reNonWord.ReplaceAllString(t, " ")) {
		b.WriteString(strings.TrimSuffix(w, "h"))
	}
	t = b.String()
	for _, r := range [][2]string{{"aa", "a"}, {"ee", "i"}, {"oo", "u"}, {"ou", "u"}, {"e", ""}} {
		t = strings.ReplaceAll(t, r[0], r[1])
	}
	return t
}

// KeyFa folds a Persian title: Arabic letter forms become Persian ones, and
// spaces, zero-width non-joiners and punctuation are dropped.
func KeyFa(title string) string {
	var b strings.Builder
	for _, r := range title {
		switch r {
		case 'ي', 'ى':
			r = 'ی'
		case 'ك':
			r = 'ک'
		case 'ة':
			r = 'ه'
		case 'أ', 'إ', 'آ':
			r = 'ا'
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Slug turns a title into a URL-friendly id.
func Slug(title string) string {
	s := strings.Trim(reSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if s == "" {
		s = "untitled"
	}
	return s
}

var (
	reSeasonNum  = regexp.MustCompile(`(?i)season\D{0,3}(\d+)`)
	reEpisodeNum = regexp.MustCompile(`(?i)^\s*(?:e|é|ep|episode|episdoe)?\s*[-.]?\s*(\d+)`)
)

// SeasonNumber reads the number in a season name such as "Season 02" or
// "Vol 1 Season 2"; it returns 0 if there is none.
func SeasonNumber(name string) int {
	if m := reSeasonNum.FindStringSubmatch(name); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// EpisodeNumber reads the number at the start of an episode title such as
// "E01", "E12 - End" or "Episode 3"; it returns 0 if there is none.
func EpisodeNumber(title string) int {
	if m := reEpisodeNum.FindStringSubmatch(title); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}
