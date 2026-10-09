package rolemeta

import (
	"maps"
	"regexp"
	"testing"
)

func TestSchemaFitsDiscord(t *testing.T) {
	if len(Schema) > 5 {
		t.Fatalf("Discord allows five records, the schema has %d", len(Schema))
	}
	key := regexp.MustCompile(`^[a-z0-9_]{1,50}$`)
	seen := map[string]bool{}
	for _, r := range Schema {
		if !key.MatchString(r.Key) || seen[r.Key] {
			t.Errorf("key %q: must be 1-50 of a-z0-9_ and unique", r.Key)
		}
		seen[r.Key] = true
		if len(r.Name) < 1 || len(r.Name) > 100 || len(r.Description) < 1 || len(r.Description) > 200 {
			t.Errorf("%s: name 1-100, description 1-200 characters", r.Key)
		}
		if r.Type != BooleanEqual && r.Type != IntegerGreaterThanOrEqual {
			t.Errorf("%s: unexpected type %d", r.Key, r.Type)
		}
		if (r.Provider != "") != (r.Type == BooleanEqual) {
			t.Errorf("%s: a boolean record follows a provider, and only it", r.Key)
		}
	}
}

func TestValues(t *testing.T) {
	want := map[string]string{"steam_linked": "1", "xbox_linked": "0", "rsi_linked": "0", "pubg_linked": "0", "supporter_tier": "0"}
	if got := Values([]string{"discord", "steam"}, 0); !maps.Equal(got, want) {
		t.Errorf("discord+steam: %v", got)
	}
	none := Values(nil, 2)
	if none["steam_linked"] != "0" || none["supporter_tier"] != "2" || len(none) != len(Schema) {
		t.Errorf("no providers, tier 2: %v", none)
	}
}
