package news

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	// DescriptionMax is where an embed's notes are cut: Discord allows 4096, and patch notes past
	// this link to Steam instead.
	DescriptionMax = 1800
	// clanImages is what Steam's {STEAM_CLAN_IMAGE} placeholder stands for.
	clanImages = "https://clan.akamai.steamstatic.com/images"
)

// Stand-ins for Valve's escaped brackets and for list bullets, so neither the Markdown escaping nor
// the tag stripping touches them.
const (
	standinL      = "\u0001"
	standinR      = "\u0002"
	standinBullet = "\u0003"
)

var (
	// mediaOpen is a media tag whose whole block is dropped (the embed shows the first image).
	mediaOpen  = regexp.MustCompile(`(?i)\[(img|video|carousel|previewyoutube)\b[^\]]*\]`)
	mediaClose = map[string]*regexp.Regexp{
		"img":            regexp.MustCompile(`(?i)\[/img\]`),
		"video":          regexp.MustCompile(`(?i)\[/video\]`),
		"carousel":       regexp.MustCompile(`(?i)\[/carousel\]`),
		"previewyoutube": regexp.MustCompile(`(?i)\[/previewyoutube\]`),
	}
	imgOpen    = regexp.MustCompile(`(?i)\[img\b[^\]]*\]`)
	urlTag     = regexp.MustCompile(`(?i)\[url=["']?([^\]"']+)["']?\]([\s\S]*?)\[/url\]`)
	unescape   = regexp.MustCompile("\\\\([*_~`|])")
	heading    = regexp.MustCompile(`(?i)\[h[1-6]\]([\s\S]*?)\[/h[1-6]\]`)
	bold       = regexp.MustCompile(`(?i)\[b\]([\s\S]*?)\[/b\]`)
	italic     = regexp.MustCompile(`(?i)\[i\]([\s\S]*?)\[/i\]`)
	strike     = regexp.MustCompile(`(?i)\[strike\]([\s\S]*?)\[/strike\]`)
	closeBlock = regexp.MustCompile(`(?i)\[/(p|tr|list)\]`)
	closeCell  = regexp.MustCompile(`(?i)\[/t[dh]\]`)
	// tag is every other Steam BBCode tag, opening or closing, with or without attributes. Only
	// these are stripped, so bracketed text such as a "[1]" footnote survives.
	tag         = regexp.MustCompile(`(?i)\[/?(?:p|list|olist|h[1-6]|b|i|u|s|strike|url|img|video|carousel|previewyoutube|table|tr|td|th|quote|code|spoiler|noparse|hr|dynamiclink)\b(?:=[^\]]*)?(?:\s[^\]]*)?\]`)
	spaces      = regexp.MustCompile(`[ \t]+`)
	lineMarkup  = regexp.MustCompile(`^([#>-] )`)
	emptyBullet = regexp.MustCompile(`(?m)^• *$`)
	blankLines  = regexp.MustCompile(`\n{3,}`)
	firstImg    = regexp.MustCompile(`(?i)\[img(?:\s+src=["']?([^\]"']+)["']?)?\]([^\[]*)`)

	// markdownChars are the characters Discord reads as inline formatting, backslashed so
	// patch-note text stays literal.
	markdownChars = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `_`, `\_`, `~`, `\~`, "`", "\\`", `|`, `\|`)
)

// ToMarkdown turns Steam's BBCode into Discord Markdown. It covers the tags game announcements use
// (surveyed over Counter-Strike 2's last 100 on 2026-10-05): p, list/*, h1 to h6, b, i, strike,
// url, img, video, carousel, previewyoutube, table rows. Media is dropped (the embed shows the
// first image), and any other known tag is stripped.
func ToMarkdown(bbcode string) string {
	s := strings.NewReplacer(`\[`, standinL, `\]`, standinR, "[*]", standinBullet, "[/*]", "").Replace(bbcode)
	s = markdownChars.Replace(s)
	s = dropMedia(s)
	s = imgOpen.ReplaceAllString(s, "")
	s = urlTag.ReplaceAllStringFunc(s, func(m string) string {
		g := urlTag.FindStringSubmatch(m)
		href := unescape.ReplaceAllString(g[1], "$1")
		if label := trim(g[2]); label != "" {
			return "[" + label + "](" + href + ")"
		}
		return href
	})
	s = heading.ReplaceAllString(s, "\n**${1}**\n\n")
	s = bold.ReplaceAllString(s, "**${1}**")
	s = italic.ReplaceAllString(s, "*${1}*")
	s = strike.ReplaceAllString(s, "~~${1}~~")
	s = closeBlock.ReplaceAllString(s, "\n")
	s = closeCell.ReplaceAllString(s, " ")
	s = tag.ReplaceAllString(s, "")
	s = strings.NewReplacer(standinBullet, "\n• ", standinL, "[", standinR, "]").Replace(s)

	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = trim(spaces.ReplaceAllString(l, " "))
		// A line Discord would read as a heading, quote or list item stays plain text.
		lines[i] = lineMarkup.ReplaceAllString(l, `\${1}`)
	}
	s = strings.Join(lines, "\n")
	s = emptyBullet.ReplaceAllString(s, "")
	s = blankLines.ReplaceAllString(s, "\n\n")
	// Each list item is its own paragraph in Steam's markup; keep a list tight.
	lines = strings.Split(s, "\n")
	tight := lines[:0:0]
	for i, l := range lines {
		if l == "" && i > 0 && i+1 < len(lines) && strings.HasPrefix(lines[i-1], "• ") && strings.HasPrefix(lines[i+1], "• ") {
			continue
		}
		tight = append(tight, l)
	}
	return trim(strings.Join(tight, "\n"))
}

// dropMedia removes each media block, from its opening tag to the first closing tag of the same
// name; an opening tag with no close is left for the tag stripping.
func dropMedia(s string) string {
	from := 0
	for from < len(s) {
		loc := mediaOpen.FindStringSubmatchIndex(s[from:])
		if loc == nil {
			break
		}
		start, end := from+loc[0], from+loc[1]
		name := strings.ToLower(s[from+loc[2] : from+loc[3]])
		if c := mediaClose[name].FindStringIndex(s[end:]); c != nil {
			s = s[:start] + s[end+c[1]:]
			from = start
			continue
		}
		from = start + 1
	}
	return s
}

// Fit cuts text at a line boundary under DescriptionMax and points to the full post.
func Fit(text, link string) string {
	r := []rune(text)
	if len(r) <= DescriptionMax {
		return text
	}
	cut := -1
	for i := min(DescriptionMax, len(r)-1); i >= 0; i-- {
		if r[i] == '\n' {
			cut = i
			break
		}
	}
	if cut < DescriptionMax/2 {
		cut = DescriptionMax
	}
	return strings.TrimRightFunc(string(r[:cut]), isSpace) + "\n\n… [Full notes on Steam](" + link + ")"
}

// FirstImage is the post's first image as an https URL, with Steam's clan placeholder filled in;
// "" when there is none.
func FirstImage(bbcode string) string {
	m := firstImg.FindStringSubmatch(bbcode)
	if m == nil {
		return ""
	}
	src := m[1]
	if src == "" {
		src = m[2]
	}
	src = strings.Replace(trim(src), "{STEAM_CLAN_IMAGE}", clanImages, 1)
	if !strings.HasPrefix(src, "https://") {
		return ""
	}
	return src
}

// trim is JavaScript's String.prototype.trim, which the Worker's text went through.
func trim(s string) string { return strings.TrimFunc(s, isSpace) }

func isSpace(r rune) bool { return unicode.IsSpace(r) || r == 0xFEFF }
