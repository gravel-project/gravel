package wardogs

import (
	"slices"
	"strings"
)

// Doc is a configuration document parsed for editing: its sections in order, each with its lines
// in order, so a document that is parsed and written back is the same document (line endings
// aside: String writes CRLF, as the server expects). Unreal's array syntax is kept per line:
// `!Key=ClearArray` empties an array, `.Key=value` (or `+Key=value`) appends, `-Key=value` removes.
type Doc struct {
	// Preamble are the lines before the first section header (comments, blanks).
	Preamble []string
	Sections []DocSection
}

// DocSection is one `[Name]` and its lines.
type DocSection struct {
	Name  string
	Lines []DocLine
}

// DocLine is one line of a section: a key line (Key set) or anything else (Raw only: a blank, a
// comment).
type DocLine struct {
	// Op is the array prefix: "", ".", "+", "-" or "!".
	Op    string
	Key   string
	Value string
	// Raw is the line as read, without its ending; String writes it for a line Set did not make.
	Raw string
}

// ParseDoc reads a configuration document; it never fails, a line it cannot read is kept raw.
func ParseDoc(text string) Doc {
	var d Doc
	var cur *DocSection
	for line := range strings.SplitSeq(NormalizeEOL(text), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			d.Sections = append(d.Sections, DocSection{Name: strings.TrimSpace(t[1 : len(t)-1])})
			cur = &d.Sections[len(d.Sections)-1]
			continue
		}
		if cur == nil {
			d.Preamble = append(d.Preamble, line)
			continue
		}
		dl := DocLine{Raw: line}
		if k, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(t, ";") && !strings.HasPrefix(t, "#") {
			k = strings.TrimSpace(k)
			if k != "" && strings.ContainsAny(k[:1], ".+-!") {
				dl.Op, k = k[:1], strings.TrimSpace(k[1:])
			}
			if k != "" {
				dl.Key, dl.Value = k, strings.TrimSpace(v)
			}
		}
		cur.Lines = append(cur.Lines, dl)
	}
	// A trailing newline leaves one empty line; keep documents stable across a round trip.
	if n := len(d.Sections); n > 0 {
		s := &d.Sections[n-1]
		for len(s.Lines) > 0 && s.Lines[len(s.Lines)-1].Key == "" && strings.TrimSpace(s.Lines[len(s.Lines)-1].Raw) == "" {
			s.Lines = s.Lines[:len(s.Lines)-1]
		}
	}
	return d
}

// String writes the document with CRLF line endings and a blank line between sections.
func (d Doc) String() string {
	var b strings.Builder
	for _, l := range d.Preamble {
		b.WriteString(l + "\r\n")
	}
	for i, s := range d.Sections {
		if i > 0 && !endsBlank(d.Sections[i-1]) {
			b.WriteString("\r\n")
		}
		b.WriteString("[" + s.Name + "]\r\n")
		for _, l := range s.Lines {
			b.WriteString(l.String() + "\r\n")
		}
	}
	return b.String()
}

func endsBlank(s DocSection) bool {
	return len(s.Lines) > 0 && s.Lines[len(s.Lines)-1].Key == "" && strings.TrimSpace(s.Lines[len(s.Lines)-1].Raw) == ""
}

func (l DocLine) String() string {
	if l.Raw != "" || l.Key == "" {
		return l.Raw
	}
	return l.Op + l.Key + "=" + l.Value
}

// Section finds a section by name; nil when the document has none.
func (d *Doc) Section(name string) *DocSection {
	for i := range d.Sections {
		if d.Sections[i].Name == name {
			return &d.Sections[i]
		}
	}
	return nil
}

// Value is a plain key's value (the last one, as Unreal reads it); ok is false when absent.
func (s *DocSection) Value(key string) (string, bool) {
	v, ok := "", false
	for _, l := range s.Lines {
		if l.Op == "" && strings.EqualFold(l.Key, key) {
			v, ok = l.Value, true
		}
	}
	return v, ok
}

// Array is an array key's values in order, as the `!`/`.`/`+`/`-` lines leave it.
func (s *DocSection) Array(key string) []string {
	var out []string
	for _, l := range s.Lines {
		if !strings.EqualFold(l.Key, key) {
			continue
		}
		switch l.Op {
		case "!":
			out = nil
		case ".", "+":
			out = append(out, unquote(l.Value))
		case "-":
			out = slices.DeleteFunc(out, func(v string) bool { return v == unquote(l.Value) })
		}
	}
	return out
}

// HasKey reports whether any line of the section names the key, with any prefix.
func (s *DocSection) HasKey(key string) bool {
	return slices.ContainsFunc(s.Lines, func(l DocLine) bool { return strings.EqualFold(l.Key, key) })
}

// SetArray replaces every line of an array key with `!Key=ClearArray` and one quoted `.Key=` line
// per value, where the key's first line was (or at the end of the section).
func (s *DocSection) SetArray(key string, values []string) {
	at := -1
	var kept []DocLine
	for _, l := range s.Lines {
		if strings.EqualFold(l.Key, key) {
			if at < 0 {
				at = len(kept)
			}
			continue
		}
		kept = append(kept, l)
	}
	if at < 0 {
		at = len(kept)
		for at > 0 && kept[at-1].Key == "" && strings.TrimSpace(kept[at-1].Raw) == "" {
			at-- // before the section's trailing blank lines
		}
	}
	block := []DocLine{{Op: "!", Key: key, Value: "ClearArray"}}
	for _, v := range values {
		block = append(block, DocLine{Op: ".", Key: key, Value: `"` + v + `"`})
	}
	s.Lines = slices.Concat(kept[:at], block, kept[at:])
}

// SetSection replaces a section with another's lines, or appends it when the document lacks it.
func (d *Doc) SetSection(s DocSection) {
	if cur := d.Section(s.Name); cur != nil {
		cur.Lines = slices.Clone(s.Lines)
		return
	}
	d.Sections = append(d.Sections, DocSection{Name: s.Name, Lines: slices.Clone(s.Lines)})
}

func unquote(v string) string {
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return v[1 : len(v)-1]
	}
	return v
}

// KeyChange is one key whose lines differ between two documents.
type KeyChange struct {
	Section string
	Key     string
	// Before and After are the key's lines' values in order (with their array prefix), empty
	// when the key is absent on that side.
	Before, After []string
}

// DiffDocs compares two documents key by key, sections in the newer's order then the removed.
// Values are compared as written; pass redacted documents to keep secrets out of a diff.
func DiffDocs(older, newer Doc) []KeyChange {
	lines := func(d Doc) (map[string][]string, []string) {
		m, order := map[string][]string{}, []string{}
		for _, s := range d.Sections {
			for _, l := range s.Lines {
				if l.Key == "" {
					continue
				}
				id := s.Name + "\x00" + strings.ToLower(l.Key)
				if _, seen := m[id]; !seen {
					order = append(order, id)
				}
				m[id] = append(m[id], l.Op+l.Value)
			}
		}
		return m, order
	}
	keyName := func(d Doc, id string) string {
		sec, lk, _ := strings.Cut(id, "\x00")
		for _, s := range d.Sections {
			if s.Name == sec {
				for _, l := range s.Lines {
					if strings.ToLower(l.Key) == lk {
						return l.Key
					}
				}
			}
		}
		return lk
	}
	o, _ := lines(older)
	n, order := lines(newer)
	var out []KeyChange
	for _, id := range order {
		if !slices.Equal(o[id], n[id]) {
			sec, _, _ := strings.Cut(id, "\x00")
			out = append(out, KeyChange{Section: sec, Key: keyName(newer, id), Before: o[id], After: n[id]})
		}
	}
	_, oorder := lines(older)
	for _, id := range oorder {
		if _, ok := n[id]; !ok {
			sec, _, _ := strings.Cut(id, "\x00")
			out = append(out, KeyChange{Section: sec, Key: keyName(older, id), Before: o[id]})
		}
	}
	return out
}
