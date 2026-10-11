package wardogsim

import (
	"fmt"
	"math"
	"math/rand/v2"
)

// StandInSteamIDPrefix starts every SteamID the simulator and the demo seed make up. It is the
// recorder's stand-in range (games/wardogs/record): below the first individual Steam account
// (76561197960265728), so none is a real person, and the recorder keeps it as it is.
const StandInSteamIDPrefix = "76561190000000"

// SteamID is the n-th made-up player's SteamID64 (n from 1 to MaxRegulars). The simulator's
// regulars are 1 to the profile's Regulars, so a seeded member who holds one of them plays on
// the simulated server too.
func SteamID(n int) string {
	if n < 1 || n > MaxRegulars {
		panic(fmt.Sprintf("wardogsim: stand-in player %d is not 1–%d", n, MaxRegulars))
	}
	return fmt.Sprintf("%s%03d", StandInSteamIDPrefix, n)
}

// Name is the n-th made-up player's in-game name: a gamer tag built from two word lists, the
// same for the same n, distinct for every n up to MaxRegulars. The words are invented for the
// demo and are nobody's.
func Name(n int) string {
	if n < 1 || n > MaxRegulars {
		panic(fmt.Sprintf("wardogsim: stand-in player %d is not 1–%d", n, MaxRegulars))
	}
	// A permutation of 0..1023 (389 is coprime with 1024), so neighbours get unrelated names.
	p := ((n-1)*389 + 17) % (len(tagFirst) * len(tagSecond))
	name := tagFirst[p%len(tagFirst)] + tagSecond[p/len(tagFirst)]
	if p%4 == 0 {
		name += fmt.Sprint(10 + p%90)
	}
	return name
}

var tagFirst = []string{
	"Ash", "Blitz", "Bolt", "Brick", "Chrome", "Cinder", "Dash", "Delta", "Drift", "Echo", "Flint", "Frost",
	"Ghost", "Grit", "Havoc", "Hex", "Jinx", "Kilo", "Lynx", "Mako", "Nova", "Onyx", "Pike", "Quill",
	"Rook", "Rust", "Sable", "Scout", "Tango", "Volt", "Wraith", "Zulu",
}

var tagSecond = []string{
	"Anvil", "Badger", "Bishop", "Breaker", "Byte", "Comet", "Crow", "Dagger", "Dog", "Fang", "Fox", "Gunner",
	"Hammer", "Hawk", "Jack", "Kite", "Lance", "Mantis", "Moth", "Otter", "Pilot", "Raptor", "Rider", "Runner",
	"Shade", "Shot", "Spark", "Storm", "Talon", "Viper", "Wolf", "Wren",
}

// Player is a made-up regular as the simulator and the seed see them: who they are and how they
// play.
type Player struct {
	N       int
	SteamID string
	Name    string
	// Skill multiplies the profile's kill rate; it also makes a player somewhat harder to kill.
	Skill float64
	// Activity weighs how often the player is the one who joins: a few play much more.
	Activity float64
	PingMs   int
}

// Regulars are the profile's players, drawn from rng: skill log-normal with the profile's
// spread, activity log-normal, so a few play several times as much as most.
func Regulars(p Profile, rng *rand.Rand) []Player {
	out := make([]Player, p.Regulars)
	for i := range out {
		n := i + 1
		out[i] = Player{
			N: n, SteamID: SteamID(n), Name: Name(n),
			Skill:    math.Exp(rng.NormFloat64()*p.SkillSpread - p.SkillSpread*p.SkillSpread/2),
			Activity: math.Exp(rng.NormFloat64() * 0.7),
			PingMs:   15 + rng.IntN(110),
		}
	}
	return out
}

// Poisson draws a Poisson count with mean lambda (Knuth's method for small means, a normal
// approximation above 30).
func Poisson(rng *rand.Rand, lambda float64) int {
	if lambda <= 0 {
		return 0
	}
	if lambda > 30 {
		return max(0, int(math.Round(lambda+rng.NormFloat64()*math.Sqrt(lambda))))
	}
	l, k, prod := math.Exp(-lambda), 0, 1.0
	for {
		prod *= rng.Float64()
		if prod <= l {
			return k
		}
		k++
	}
}
