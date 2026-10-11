// Package cs2 is gravel's Counter-Strike 2 package: the Game spec (game.yaml) and the client
// library the cloud-VM driver (drivers/cloudvm, ADR-0003) controls a CS2 server with. RCON (Client)
// reads "status" and sends kicks, chat and map changes; the log stream the server pushes
// (logaddress_add_http) keeps the Roster, because CS2's status lists no SteamIDs. It imports
// nothing of gravel's: at the open-source cut it becomes a module of its own.
package cs2
