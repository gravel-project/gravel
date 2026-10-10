package cs2

import _ "embed"

// GameSpec is the Counter-Strike 2 Game spec (game.yaml): its id, the identity its players are keyed by and
// the publisher's rules (gravel#8). gravel parses it; this package only ships it, so the spec
// moves with the game's code at the open-source cut.
//
//go:embed game.yaml
var GameSpec []byte
