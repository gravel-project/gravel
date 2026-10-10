package wardogs

import _ "embed"

// GameSpec is the War Dogs Game spec (game.yaml): its id, the identity its players are keyed by,
// the drivers that control it and the publisher's bands. gravel parses it; this package only
// ships it, so the spec moves with the client at the open-source cut.
//
//go:embed game.yaml
var GameSpec []byte
