# War Dogs feed batches

One directory per build, each file one batch body as the hub stores it.

- `CL-516586/server-start.json`: the first batch Hidden Token Gaming's server ever stored
  (2026-10-18 17:04Z, Season 02, at the server's start with nobody on), **reconstructed from the
  notes taken on the host**: the shape, the types, the map, the states, the server name and the
  first event's time are the capture's; the server id is the recorder's stand-in, and the event
  ids and the second event's time are stand-ins too. The raw export stays on the host because it
  holds the server's id; the next fixtures come from `gravel-hub ingest export --redact`, which
  replaces it and every SteamID before a batch leaves the host.
