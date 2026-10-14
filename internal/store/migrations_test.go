package store

import "testing"

// Every migration says what it costs (gravel#214): Migrations fails on one that doesn't, and
// migrate --check prints the cost of each pending one before an upgrade applies it.
func TestEveryMigrationHasACost(t *testing.T) {
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations embedded")
	}
	for i, m := range all {
		if m.Version != int64(i+1) {
			t.Errorf("migration %d is version %d: versions run 1, 2, 3… with no gap", i+1, m.Version)
		}
		if m.Name == "" {
			t.Errorf("migration %d has no name", m.Version)
		}
	}
	if m := all[28]; m.Name != "ownership_transfer" || m.Cost != "scan organizations" {
		t.Errorf("00029: %+v", m)
	}
}

func TestCostSyntax(t *testing.T) {
	for cost, ok := range map[string]bool{
		"instant":                       true,
		"scan users":                    true,
		"scan users, role_changes":      true,
		"rewrite match_stats":           true,
		"":                              false,
		"fast":                          false,
		"scan":                          false,
		"scan Users":                    false,
		"scan users,role_changes":       false,
		"instant users":                 false,
		"rewrite match_stats; drop all": false,
	} {
		if got := costRE.MatchString(cost); got != ok {
			t.Errorf("%q: valid %t, want %t", cost, got, ok)
		}
	}
}
