// Package rolemeta is the Linked Roles metadata gravel publishes for a member (ADR-0008 §4): the
// schema the bot registers with the Discord application, and the values the hub's verification
// flow writes for one member. One definition, so the two sides cannot disagree. It imports
// nothing from a Discord library, because the hub uses it too.
package rolemeta

import "strconv"

// Type is how a guild's role requirement compares a record (Discord's metadata types).
type Type int

// The types the schema uses.
const (
	IntegerGreaterThanOrEqual Type = 2
	BooleanEqual              Type = 7
)

// Record is one metadata field. Discord allows five per application; a key is 1 to 50
// characters of a-z, 0-9 and _.
type Record struct {
	Key         string
	Name        string
	Description string
	Type        Type
	// Provider is the identity provider a boolean record follows; empty for the others.
	Provider string
}

// Schema is the metadata every gravel bot registers. A provider the hub does not offer yet
// (Xbox, RSI, PUBG) reads 0 until it does; supporter_tier reads 0 until supporters (plan P6).
var Schema = []Record{
	{Key: "steam_linked", Name: "Steam linked", Description: "Has linked a Steam account", Type: BooleanEqual, Provider: "steam"},
	{Key: "xbox_linked", Name: "Xbox linked", Description: "Has linked an Xbox account", Type: BooleanEqual, Provider: "xbox"},
	{Key: "rsi_linked", Name: "RSI linked", Description: "Has linked a Roberts Space Industries account", Type: BooleanEqual, Provider: "rsi"},
	{Key: "pubg_linked", Name: "PUBG linked", Description: "Has linked a PUBG account", Type: BooleanEqual, Provider: "pubg"},
	{Key: "supporter_tier", Name: "Supporter tier", Description: "Supporter tier, 0 for none", Type: IntegerGreaterThanOrEqual},
}

// Values are one member's metadata: each provider record is "1" when the member linked that
// provider and "0" otherwise, supporter_tier is the tier. Discord takes every value as a string.
func Values(providers []string, supporterTier int) map[string]string {
	linked := map[string]bool{}
	for _, p := range providers {
		linked[p] = true
	}
	out := make(map[string]string, len(Schema))
	for _, r := range Schema {
		switch {
		case r.Provider != "" && linked[r.Provider]:
			out[r.Key] = "1"
		case r.Provider != "":
			out[r.Key] = "0"
		case r.Key == "supporter_tier":
			out[r.Key] = strconv.Itoa(supporterTier)
		}
	}
	return out
}
