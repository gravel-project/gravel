package stats

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/gravel-project/gravel/internal/store"
)

// Unnamed is how a player without a pseudonym is shown when the hub has no pseudonym key: no
// name is made up without one, since a pseudonym is kept forever.
const Unnamed = "Unnamed player"

// Deleted is how an erased player is shown (ADR-0012 §7): their numbers stay, their identity does not.
const Deleted = "Deleted player"

// maxSuffix is how far a pseudonym taken by another identity is numbered ("Brave Falcon 2").
const maxSuffix = 99

// Namer gives unlinked players their pseudonyms (README, Player privacy; ADR-0012 §5): two words
// chosen by a keyed hash of the identity, stored the first time and never changed, unique in the
// organization, not reversible to the identity without the key.
type Namer struct {
	st     Store
	orgID  uuid.UUID
	key    []byte
	now    func() time.Time
	logger *slog.Logger
	warn   sync.Once
}

// NewNamer builds a Namer; an empty key makes none (players without one show as Unnamed).
func NewNamer(st Store, orgID uuid.UUID, key []byte, logger *slog.Logger) *Namer {
	return &Namer{st: st, orgID: orgID, key: key, now: time.Now, logger: logger}
}

// Names returns every identity's pseudonym, storing one for an identity that has none yet. An
// erased identity is shown as Deleted and never gets one.
func (n *Namer) Names(ctx context.Context, keys []store.PlayerKey) (map[store.PlayerKey]string, error) {
	got, err := n.st.Pseudonyms(ctx, n.orgID, keys)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if k.Provider == store.ErasedProvider {
			got[k] = Deleted
			continue
		}
		if _, ok := got[k]; ok {
			continue
		}
		if len(n.key) == 0 {
			n.warn.Do(func() {
				n.logger.WarnContext(ctx, "stats: no pseudonym key (stats.pseudonym_key_file in hub.yaml): unlinked players show as "+Unnamed)
			})
			got[k] = Unnamed
			continue
		}
		name, err := n.assign(ctx, k)
		if err != nil {
			return nil, err
		}
		got[k] = name
	}
	return got, nil
}

func (n *Namer) assign(ctx context.Context, k store.PlayerKey) (string, error) {
	base := Pseudonym(n.key, k)
	for i := 1; i <= maxSuffix; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s %d", base, i)
		}
		stored, err := n.st.AddPseudonym(ctx, n.orgID, k, name, n.now())
		if errors.Is(err, store.ErrPseudonymTaken) {
			continue
		}
		return stored, err
	}
	return "", fmt.Errorf("stats: pseudonym %q and its %d numbered forms are all taken", base, maxSuffix)
}

// Pseudonym is the name a key gives an identity before a collision is numbered.
func Pseudonym(key []byte, k store.PlayerKey) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(k.Provider))
	mac.Write([]byte{0})
	mac.Write([]byte(k.Subject))
	sum := mac.Sum(nil)
	adj := adjectives[binary.BigEndian.Uint32(sum[0:4])%uint32(len(adjectives))]
	noun := nouns[binary.BigEndian.Uint32(sum[4:8])%uint32(len(nouns))]
	return adj + " " + noun
}

// The word lists: plain, friendly words that read well together in any pair and say nothing
// about a person. Changing them changes only names not yet given.
var adjectives = []string{
	"Amber", "Arctic", "Ashen", "Azure", "Bold", "Brave", "Brisk", "Bronze", "Calm", "Canny",
	"Cobalt", "Copper", "Crimson", "Daring", "Dawn", "Deft", "Desert", "Dusky", "Eager", "Ember",
	"Emerald", "Fabled", "Fearless", "Fleet", "Frost", "Gallant", "Gilded", "Granite", "Grey", "Hardy",
	"Hidden", "Highland", "Hollow", "Iron", "Ivory", "Jade", "Keen", "Lunar", "Marble", "Midnight",
	"Mighty", "Misty", "Nimble", "Noble", "Northern", "Oaken", "Onyx", "Orange", "Patient", "Plucky",
	"Polar", "Proud", "Quick", "Quiet", "Rapid", "Restless", "Rocky", "Rogue", "Royal", "Rugged",
	"Rusty", "Sable", "Saffron", "Scarlet", "Shadow", "Sharp", "Silent", "Silver", "Sly", "Solar",
	"Southern", "Spry", "Steady", "Steel", "Stern", "Stone", "Storm", "Sturdy", "Sunny", "Swift",
	"Tawny", "Thunder", "Timber", "Tireless", "Topaz", "Trusty", "Twilight", "Valiant", "Velvet", "Verdant",
	"Vigilant", "Violet", "Wandering", "Wary", "Wild", "Windy", "Winter", "Wise", "Witty", "Zealous",
}

var nouns = []string{
	"Badger", "Bear", "Beacon", "Bison", "Boar", "Bobcat", "Buffalo", "Buzzard", "Canyon", "Cardinal",
	"Cedar", "Cobra", "Comet", "Condor", "Cougar", "Coyote", "Crane", "Crow", "Dingo", "Eagle",
	"Elk", "Falcon", "Ferret", "Finch", "Fox", "Gecko", "Glacier", "Goshawk", "Griffin", "Grizzly",
	"Harrier", "Hawk", "Heron", "Hornet", "Husky", "Ibex", "Jackal", "Jaguar", "Kestrel", "Kite",
	"Koala", "Lark", "Leopard", "Lion", "Lynx", "Magpie", "Mako", "Mammoth", "Marlin", "Marten",
	"Meteor", "Mongoose", "Moose", "Mustang", "Narwhal", "Ocelot", "Orca", "Osprey", "Otter", "Owl",
	"Panther", "Pelican", "Peregrine", "Pike", "Puma", "Python", "Quail", "Raven", "Ridge", "Robin",
	"Sable", "Salmon", "Shark", "Sparrow", "Stag", "Stallion", "Starling", "Stingray", "Summit", "Swallow",
	"Swift", "Tern", "Thrush", "Tiger", "Tortoise", "Trout", "Tundra", "Viper", "Vulture", "Walrus",
	"Warbler", "Wasp", "Weasel", "Whale", "Wildcat", "Wolf", "Wolverine", "Wombat", "Wren", "Yak",
}
