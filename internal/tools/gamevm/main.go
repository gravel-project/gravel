// Command gamevm drives a Counter-Strike 2 host in a local VM (deploy/game-vm.sh) through the
// cloud-VM driver's own code: the document the hub boots an instance with, the environment file
// and the SSH push it sends, the game's RCON status, and the log route's redaction and roster. It
// proves on a developer's machine what the hub does to a cloud VM, without a cloud account.
//
//	go run ./internal/tools/gamevm document --device DEV --ssh-key PUB [--name N]
//	go run ./internal/tools/gamevm push --ssh HOST:PORT --key PRIV --rcon-password PW --feed URL [--max-players N]
//	go run ./internal/tools/gamevm watch --rcon HOST:PORT --rcon-password PW --listen ADDR --token T [--timeout D] [--batches N]
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/gravel-project/gravel/drivers/cloudvm"
	cs2game "github.com/gravel-project/gravel/drivers/cloudvm/cs2"
	"github.com/gravel-project/gravel/games/cs2"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: gamevm document|push|watch [flags]")
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "document":
		err = document(os.Args[2:])
	case "push":
		err = push(ctx, os.Args[2:])
	case "watch":
		err = watch(ctx, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gamevm:", err)
		os.Exit(1)
	}
}

// profile is the cloudvm section gravel ships for CS2.
func profile() (cloudvm.Profile, error) {
	var spec struct {
		Name    string          `yaml:"name"`
		CloudVM cloudvm.Profile `yaml:"cloudvm"`
	}
	if err := yaml.Unmarshal(cs2.GameSpec, &spec); err != nil {
		return cloudvm.Profile{}, err
	}
	return spec.CloudVM, spec.CloudVM.Validate()
}

func spec(name string, settings map[string]string) (cloudvm.ServerSpec, error) {
	p, err := profile()
	if err != nil {
		return cloudvm.ServerSpec{}, err
	}
	return cloudvm.ServerSpec{ID: name, Name: "gravel drill " + name, Game: "cs2", GameName: "Counter-Strike 2", Profile: p, Settings: settings}, nil
}

func document(args []string) error {
	fs := flag.NewFlagSet("document", flag.ContinueOnError)
	device := fs.String("device", "", "the data disk's /dev path inside the VM")
	key := fs.String("ssh-key", "", "the public key file to authorize (the hub's, in a cloud)")
	name := fs.String("name", "game-vm", "the server id (the hostname)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pub, err := os.ReadFile(*key)
	if err != nil {
		return err
	}
	s, err := spec(*name, nil)
	if err != nil {
		return err
	}
	doc, err := cloudvm.Document(s, s.GameName, *device, []string{strings.TrimSpace(string(pub))}, "")
	if err != nil {
		return err
	}
	fmt.Print(doc)
	return nil
}

func push(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	target := fs.String("ssh", "127.0.0.1:2223", "the VM's SSH address")
	keyFile := fs.String("key", "", "the private key the VM authorizes (ed25519)")
	rcon := fs.String("rcon-password", "", "the RCON password to give the game")
	feed := fs.String("feed", "", "the URL the game pushes its log to (the hub's log route with the token)")
	maxPlayers := fs.Int("max-players", 10, "max_players")
	token := fs.String("publisher-token", "", "a game server login token to give the game (to check it stays out of the journal, #170)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pem, err := os.ReadFile(*keyFile)
	if err != nil {
		return err
	}
	signer, err := cloudvm.ParseSSHKey(pem)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(*target)
	if err != nil {
		return err
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return err
	}
	s, err := spec("game-vm", map[string]string{"max_players": strconv.Itoa(*maxPlayers)})
	if err != nil {
		return err
	}
	s.RCONPassword, s.FeedURL, s.PublisherToken = *rcon, *feed, *token
	env, err := cloudvm.RenderEnv(s, s.Size)
	if err != nil {
		return err
	}
	out, hostKey, err := cloudvm.SSH{}.Run(ctx, cloudvm.RemoteTarget{Address: host, Port: p, User: "gravel", Signer: signer}, bytes.NewReader(env), cloudvm.EnvScript("cs2", s.Profile.Hooks, true))
	if err != nil {
		return fmt.Errorf("push: %w: %s", err, out)
	}
	fmt.Printf("pushed %d bytes of env; the host presented %s\n", len(env), strings.Fields(hostKey)[0])
	return nil
}

// watch serves the log route (redacted, read into the roster) and polls RCON until the game
// answers, then waits for log batches.
func watch(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	rcon := fs.String("rcon", "127.0.0.1:27115", "the game's RCON address")
	password := fs.String("rcon-password", "", "the RCON password")
	listen := fs.String("listen", "127.0.0.1:18091", "where to serve the log route")
	token := fs.String("token", "", "the feed token in the log route's path")
	timeout := fs.Duration("timeout", 60*time.Minute, "how long to wait for the game to answer")
	batches := fs.Int("batches", 1, "log batches to wait for once the game answers")
	if err := fs.Parse(args); err != nil {
		return err
	}
	p, err := profile()
	if err != nil {
		return err
	}
	g, err := cs2game.New(*rcon, *password, p, slog.Default())
	if err != nil {
		return err
	}
	var mu sync.Mutex
	got := 0
	headersSeen := map[string]bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+cloudvm.IngestPath+"{token}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("token") != *token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 256<<10))
		red := cs2.RedactLog(body, *token)
		for _, h := range cs2.LogHeaders {
			if v := r.Header.Get(h); v != "" {
				headersSeen[h] = true
			}
		}
		joined := g.Log(red)
		mu.Lock()
		got++
		n := got
		mu.Unlock()
		lines := strings.Count(string(red), "\n")
		fmt.Printf("log batch %d: %d bytes, %d lines, content-type %q, user-agent %q, headers %s\n", n, len(body), lines,
			r.Header.Get("Content-Type"), r.Header.Get("User-Agent"), headerList(r.Header))
		if bytes.Contains(red, []byte(*token)) {
			fmt.Println("  REDACTION FAILED: the token is in the kept batch")
		}
		for _, p := range joined.Connected {
			fmt.Printf("  connected: user %d %q as %s:%s\n", p.UserID, p.Name, p.Identity.Provider, p.Identity.Subject)
		}
		if joined.Boundary != "" {
			fmt.Printf("  boundary: %s\n", joined.Boundary)
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.ListenAndServe() }()
	defer func() { _ = srv.Close() }()

	start := time.Now()
	deadline := start.Add(*timeout)
	for {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st, err := g.Status(sctx)
		cancel()
		if err == nil {
			fmt.Printf("the game answered RCON after %s: build %q, map %q, %d humans, %d bots, %d max, hostname %q\n",
				time.Since(start).Round(time.Second), st.Build, st.Map, st.Players, st.Bots, st.MaxPlayers, st.Hostname)
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the game did not answer RCON in %s: %w", *timeout, err)
		}
		time.Sleep(15 * time.Second)
	}
	raw, err := cs2.New(*rcon, *password, cs2.Options{})
	if err != nil {
		return err
	}
	out, err := raw.Exec(ctx, "status")
	if err != nil {
		return err
	}
	fmt.Printf("raw status (%d bytes):\n%s\n", len(out), out)
	if err := g.Say(ctx, "gravel drill: hello"); err != nil {
		return fmt.Errorf("say: %w", err)
	}
	for wait := time.Now().Add(5 * time.Minute); ; {
		mu.Lock()
		n := got
		mu.Unlock()
		if n >= *batches {
			break
		}
		if time.Now().After(wait) {
			return errors.New("no log batch reached the listener in 5 minutes")
		}
		time.Sleep(2 * time.Second)
	}
	var missing []string
	for _, h := range cs2.LogHeaders {
		if !headersSeen[h] {
			missing = append(missing, h)
		}
	}
	fmt.Printf("log push works; headers the game never sent: %v\n", missing)
	return nil
}

func headerList(h http.Header) string {
	var out []string
	for k := range h {
		if strings.HasPrefix(k, "X-") {
			out = append(out, k)
		}
	}
	return strings.Join(out, ",")
}
