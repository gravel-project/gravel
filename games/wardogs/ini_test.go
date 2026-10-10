package wardogs_test

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/gravel-project/gravel/games/wardogs"
)

func recordedConfig(t *testing.T) wardogs.ConfigDocument {
	t.Helper()
	raw, err := os.ReadFile("testdata/CL-509546/v1/config.json")
	if err != nil {
		t.Fatal(err)
	}
	var d wardogs.ConfigDocument
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestDocRoundTripsTheRecordedDocument(t *testing.T) {
	text := recordedConfig(t).Text
	d := wardogs.ParseDoc(text)
	if got, want := d.String(), wardogs.ToCRLF(strings.TrimRight(wardogs.NormalizeEOL(text), "\n")+"\n"); got != want {
		t.Errorf("round trip differs:\n%q\n%q", got, want)
	}
	if len(d.Sections) != 6 { // WDServerFeed is in the schema, not in this document
		t.Errorf("sections = %d", len(d.Sections))
	}
	ks := d.Section("MatchState.Playing.KOTH")
	if v, ok := ks.Value("ScorePeriod"); !ok || v != "24" {
		t.Errorf("ScorePeriod = %q, %v", v, ok)
	}
	rot := d.Section("/Script/WDGame.WDServerMapRotationSettings").Array("RotationEntries")
	if len(rot) < 3 || !strings.HasPrefix(rot[0], `(Map="Europe"`) {
		t.Errorf("rotation = %v", rot)
	}
}

func TestDocArrays(t *testing.T) {
	d := wardogs.ParseDoc("[S]\r\nA=1\r\n!Ids=ClearArray\r\n.Ids=\"1\"\r\n+Ids=\"2\"\r\n.Ids=\"3\"\r\n-Ids=\"2\"\r\nB=2\r\n\r\n[T]\r\nC=3\r\n")
	s := d.Section("S")
	if got := s.Array("Ids"); !slices.Equal(got, []string{"1", "3"}) {
		t.Errorf("array = %v", got)
	}
	s.SetArray("Ids", []string{"9", "8"})
	want := "[S]\r\nA=1\r\n!Ids=ClearArray\r\n.Ids=\"9\"\r\n.Ids=\"8\"\r\nB=2\r\n\r\n[T]\r\nC=3\r\n"
	if got := d.String(); got != want {
		t.Errorf("after SetArray:\n%q\nwant\n%q", got, want)
	}
	// A key the section lacks goes at its end, before the blank line.
	d.Section("S").SetArray("New", nil)
	if !strings.Contains(d.String(), "B=2\r\n!New=ClearArray\r\n\r\n[T]") {
		t.Errorf("appended array:\n%q", d.String())
	}
	if !d.Section("S").HasKey("new") || d.Section("T").HasKey("Ids") {
		t.Error("HasKey")
	}
}

func TestDiffDocs(t *testing.T) {
	a := wardogs.ParseDoc("[K]\r\nScorePeriod=24\r\nGone=1\r\n[S]\r\n!Ids=ClearArray\r\n.Ids=\"1\"\r\n")
	b := wardogs.ParseDoc("[K]\r\nScorePeriod=27\r\n[S]\r\n!Ids=ClearArray\r\n.Ids=\"1\"\r\n.Ids=\"2\"\r\n[N]\r\nX=1\r\n")
	got := wardogs.DiffDocs(a, b)
	if len(got) != 4 {
		t.Fatalf("diff = %+v", got)
	}
	if c := got[0]; c.Section != "K" || c.Key != "ScorePeriod" || !slices.Equal(c.Before, []string{"24"}) || !slices.Equal(c.After, []string{"27"}) {
		t.Errorf("first = %+v", c)
	}
	if c := got[1]; c.Key != "Ids" || len(c.After) != 3 {
		t.Errorf("array = %+v", c)
	}
	if c := got[2]; c.Section != "N" || c.Before != nil {
		t.Errorf("added = %+v", c)
	}
	if c := got[3]; c.Key != "Gone" || c.After != nil {
		t.Errorf("removed = %+v", c)
	}
	if d := wardogs.DiffDocs(a, a); len(d) != 0 {
		t.Errorf("self diff = %+v", d)
	}
}
