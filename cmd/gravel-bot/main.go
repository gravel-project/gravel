// Command gravel-bot runs gravel's Discord bot with the stock modules (docs/bot.md).
//
//	gravel-bot serve        [--config bot.yaml]            run the bot (default)
//	gravel-bot config check [--config bot.yaml]            load and validate the configuration
//	gravel-bot healthcheck  [--url http://127.0.0.1:8081/healthz]
//	gravel-bot version
//
// The config path defaults to $GRAVEL_BOT_CONFIG, then /etc/gravel/bot.yaml. A host's bot is
// the same Program with its own name, paths and modules (discord/bot/cli).
package main

import (
	"os"

	"github.com/gravel-project/gravel/discord/bot/cli"
)

// version is set by the linker (-X main.version=...); ko and goreleaser both do.
var version = "dev"

func program() cli.Program {
	return cli.Program{Name: "gravel-bot", Version: version, DefaultConfig: "/etc/gravel/bot.yaml", ConfigEnv: "GRAVEL_BOT_CONFIG"}
}

func main() { os.Exit(program().Main()) }
