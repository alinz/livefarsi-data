package catalog

import (
	"testing"
)

func TestKey(t *testing.T) {
	same := [][2]string{
		{"Bamdade Khomar", "Bamdad Khomar"},
		{"vahshi-2025", "Vaahshi"},
		{"Hezartoo", "Hezartou"},
		{"Sarzamine Madari", "Sarzamin Madari"},
		{"Belite Yek Tarafeh", "Belit Yek Tarafeh"},
		{"SadDam", "saddam"},
		{"Nim Rokh", "nimrokh"},
	}
	for _, p := range same {
		if Key(p[0]) != Key(p[1]) {
			t.Errorf("Key(%q) = %q, Key(%q) = %q; want equal", p[0], Key(p[0]), p[1], Key(p[1]))
		}
	}
	different := [][2]string{{"Koori", "Kar"}, {"Shah Gol", "Gole Sang"}}
	for _, p := range different {
		if Key(p[0]) == Key(p[1]) {
			t.Errorf("Key(%q) == Key(%q) = %q; want different", p[0], p[1], Key(p[0]))
		}
	}
}

func TestKeyFa(t *testing.T) {
	if KeyFa("اعتراف می کنم") != KeyFa("اعتراف می‌کنم") {
		t.Error("space and zero-width non-joiner should fold")
	}
	if KeyFa("كوري") != KeyFa("کوری") {
		t.Error("Arabic kaf and yeh should fold to Persian")
	}
}

func TestNumbers(t *testing.T) {
	seasons := map[string]int{"Season 01": 1, "Vol 1 Season 2": 2, "Season-3": 3, "Seasons 4": 4, "Episodes": 0, "Poshte Sahne": 0}
	for in, want := range seasons {
		if got := SeasonNumber(in); got != want {
			t.Errorf("SeasonNumber(%q) = %d, want %d", in, got, want)
		}
	}
	episodes := map[string]int{"E01": 1, "E12 - End": 12, "Episode 3": 3, "Episode 1": 1, "É7": 7, "Episdoe-4": 4, "05": 5, "Poshte Sahne 2": 0}
	for in, want := range episodes {
		if got := EpisodeNumber(in); got != want {
			t.Errorf("EpisodeNumber(%q) = %d, want %d", in, got, want)
		}
	}
}

func player(src, url string) []*Player { return []*Player{{Source: src, Type: "iframe", URL: url}} }

func TestMerge(t *testing.T) {
	a := &Catalog{
		Series: []*Title{
			{ID: "koori", Title: "Koori", TitleFa: "کوری", Poster: "a.jpg", Links: []Link{{"a", "a/koori"}}, Seasons: []*Season{
				{Number: 1, Name: "Season 01", Episodes: []*Episode{
					{Number: 1, Title: "E01", Players: player("a", "a1")},
					{Number: 2, Title: "E02", Players: player("a", "a2")},
				}},
			}},
			// Same key as "Badaal" within one source: must stay separate.
			{ID: "badal", Title: "Badal", Links: []Link{{"a", "a/badal"}}},
			{ID: "badaal", Title: "Badaal", Links: []Link{{"a", "a/badaal"}}},
		},
		Movies: []*Title{{ID: "kanape", Title: "Kanape", Players: player("a", "am")}},
	}
	b := &Catalog{
		Series: []*Title{
			// Different spelling, same Persian title.
			{ID: "kouri", Title: "Kouri Series", TitleFa: "كوري", Poster: "b.jpg", Description: "synopsis", Links: []Link{{"b", "b/kouri"}}, Seasons: []*Season{
				{Number: 1, Name: "Season 1", Episodes: []*Episode{
					{Number: 3, Title: "Episode 3", Players: player("b", "b3")},
					{Number: 1, Title: "Episode 1", Players: player("b", "b1")},
				}},
				{Number: 2, Name: "Season 2", Episodes: []*Episode{{Number: 1, Title: "Episode 1", Players: player("b", "b21")}}},
			}},
			{ID: "koori", Title: "Something Else", Links: []Link{{"b", "b/other"}}},
		},
		Movies: []*Title{{ID: "kanapeh", Title: "Kanapeh", Players: player("b", "bm")}},
	}

	m := Merge(a, b)
	if len(m.Series) != 4 {
		t.Fatalf("got %d series, want 4 (koori merged, badal and badaal separate, one new)", len(m.Series))
	}
	k := m.Series[0]
	if k.Poster != "a.jpg" || k.Description != "synopsis" || len(k.Links) != 2 {
		t.Errorf("merged display fields: %+v", k)
	}
	if len(k.Seasons) != 2 {
		t.Fatalf("got %d seasons, want 2", len(k.Seasons))
	}
	var got []string
	for _, ep := range k.Seasons[0].Episodes {
		s := ep.Title + ":"
		for _, p := range ep.Players {
			s += p.URL + ","
		}
		got = append(got, s)
	}
	want := []string{"E01:a1,b1,", "E02:a2,", "Episode 3:b3,"}
	if len(got) != len(want) {
		t.Fatalf("season 1 episodes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("season 1 episodes = %v, want %v", got, want)
			break
		}
	}
	if m.Series[3].ID != "koori-2" {
		t.Errorf("unmatched title with a taken id got %q, want koori-2", m.Series[3].ID)
	}
	if len(m.Movies) != 1 || len(m.Movies[0].Players) != 2 {
		t.Errorf("movies should merge into one with 2 players: %+v", m.Movies)
	}
	// Inputs are untouched.
	if len(a.Series[0].Seasons[0].Episodes[0].Players) != 1 || len(a.Series[0].Links) != 1 {
		t.Error("Merge modified its input")
	}
}
