package news_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gravel-project/gravel/discord/modules/news"
)

// The cases below are the cs-news Worker's (hidden-token-gaming/deploy workers/cs-news), whose
// posts the module continues in the same channel.

func cs2() news.Feed {
	return news.Feed{AppID: 730, Label: "Counter-Strike 2", Feeds: []string{news.DefaultFeed}, Color: 0xde9b35}
}

func TestEmbedOneLinkedDatedPost(t *testing.T) {
	items := fixture(t, "steam-news.json")
	e := news.Embed(cs2(), items[1])
	if e.Title != "Counter-Strike 2 Update" || e.URL != "https://store.steampowered.com/news/app/730/view/1845383656379136" || e.Color != 0xde9b35 {
		t.Errorf("title, link, color: %q %q %#x", e.Title, e.URL, e.Color)
	}
	if e.Timestamp == nil || !e.Timestamp.Equal(time.Unix(1790808379, 0)) || e.Footer == nil || e.Footer.Text != "Counter-Strike 2 · Steam announcement" {
		t.Errorf("timestamp and footer: %v %+v", e.Timestamp, e.Footer)
	}
	if !regexp.MustCompile(`^\[ MISC \]\n\n• Various clipping fixes in Rush maps\n\n\[ CROSSHAIR \]\n\n• Fixed visualization`).MatchString(e.Description) {
		t.Errorf("description:\n%s", e.Description)
	}
	if !strings.Contains(e.Description, "• Added crosshair outline color option\n• Added a new style \"Static Quadrant\"") {
		t.Errorf("a tight list:\n%s", e.Description)
	}
	if e.Image != nil {
		t.Errorf("no image: %+v", e.Image)
	}
	if def := news.Embed(news.Feed{AppID: 730, Label: "CS2"}, items[1]); def.Color != news.DefaultColor {
		t.Errorf("the default color: %#x", def.Color)
	}
	long := items[1]
	long.Title = strings.Repeat("é", 300)
	if got := []rune(news.Embed(cs2(), long).Title); len(got) != 256 {
		t.Errorf("a title is cut at 256 characters: %d", len(got))
	}
}

func TestEmbedARichPost(t *testing.T) {
	e := news.Embed(cs2(), fixture(t, "steam-news-rich.json")[0])
	if !strings.HasPrefix(e.Description, "Today we're introducing the Dead Hand Collection") {
		t.Errorf("starts with the text:\n%s", e.Description)
	}
	if !strings.Contains(e.Description, "\n**Mutually Assured Distraction**\n") {
		t.Errorf("keeps the heading:\n%s", e.Description)
	}
	if regexp.MustCompile(`\[/?(img|carousel|h3)|steamstatic`).MatchString(e.Description) {
		t.Errorf("drops the media:\n%s", e.Description)
	}
	if e.Image == nil || !strings.HasPrefix(e.Image.URL, "https://clan.fastly.steamstatic.com/images/3381077/55ed2cb8") {
		t.Errorf("the first image: %+v", e.Image)
	}
}

func TestEmbedCutsLongNotes(t *testing.T) {
	it := fixture(t, "steam-news.json")[0]
	var b strings.Builder
	for n := range 200 {
		fmt.Fprintf(&b, "[p]line %d of the patch notes[/p]", n)
	}
	it.Contents = b.String()
	d := news.Embed(cs2(), it).Description
	if len(d) >= 2000 {
		t.Errorf("description is %d", len(d))
	}
	if !regexp.MustCompile(`line \d+ of the patch notes\n\n… \[Full notes on Steam\]\(https://store\.steampowered\.com/news/app/730/view/\d+\)$`).MatchString(d) {
		t.Errorf("cut at a line with the link:\n%s", d[len(d)-200:])
	}
	// No line break in the second half: cut at the limit, in characters, never inside one.
	flat := news.Fit(strings.Repeat("a", 500)+"\n"+strings.Repeat("é", 2000), "https://x")
	if flat != strings.Repeat("a", 500)+"\n"+strings.Repeat("é", 1299)+"\n\n… [Full notes on Steam](https://x)" {
		t.Errorf("a cut at the limit: %q", flat[len(flat)-60:])
	}
	if short := news.Fit("short\n", "https://x"); short != "short\n" {
		t.Errorf("short text stays: %q", short)
	}
}

func TestToMarkdown(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		// Steam's tags become Discord Markdown.
		{`[h3]New[/h3][p]A [b]bold[/b], [i]slanted[/i] and [strike]old[/strike] [url="https://example.com/a_b"]link[/url].[/p]`,
			"**New**\n\nA **bold**, *slanted* and ~~old~~ [link](https://example.com/a_b)."},
		{"[list][*][p]one[/p][/*][*][p]two[/p][/*][/list]", "• one\n• two"},
		{"[table][tr][td]Map[/td][td]Pool[/td][/tr][/table]", "Map Pool"},
		{`[previewyoutube="abc;full"][/previewyoutube][p]after[/p]`, "after"},
		{`[url=https://example.com/x][/url]`, "https://example.com/x"},
		{`[P]Upper [B]case[/B][/P]`, "Upper **case**"},
		{"[carousel][img]a.png[/img][img]b.png[/img][/carousel][p]text[/p][video mp4=x][/video]", "text"},
		// Patch-note text stays literal.
		{`[p]\[ MISC ][/p]`, "[ MISC ]"},
		{"[p]set cl_crosshair_t to *1* | `x`[/p]", "set cl\\_crosshair\\_t to \\*1\\* \\| \\`x\\`"},
		{"[p]# not a heading[/p][p]- not a list[/p][p]> not a quote[/p]", "\\# not a heading\n\\- not a list\n\\> not a quote"},
		{"[p]see note [1][/p]", "see note [1]"},
	} {
		if got := news.ToMarkdown(tc.in); got != tc.want {
			t.Errorf("ToMarkdown(%q)\n got %q\nwant %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstImage(t *testing.T) {
	for in, want := range map[string]string{
		"[img]{STEAM_CLAN_IMAGE}/3381077/a.png[/img]":        "https://clan.akamai.steamstatic.com/images/3381077/a.png",
		`[img src="{STEAM_CLAN_IMAGE}/3381077/b.png"][/img]`: "https://clan.akamai.steamstatic.com/images/3381077/b.png",
		"[p]no image[/p]": "",
		"[img]http://insecure.example.com/c.png[/img]":       "",
		"[p]x[/p][IMG] https://cdn.example.com/d.png [/IMG]": "https://cdn.example.com/d.png",
	} {
		if got := news.FirstImage(in); got != want {
			t.Errorf("FirstImage(%q) = %q, want %q", in, got, want)
		}
	}
}
