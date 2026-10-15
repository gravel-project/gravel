# The gravel CLI

`gravel` is the operator's command line. Its `hub` commands make and look after the hub's own
host through a cloud provider ([ADR-0020](adr/0020-the-hubs-own-host-made-by-the-cli-through-the-cloud-provider.md)).
The commands that act on a running hub (`login`, `claim`, `whoami`, `fleet`, `servers`, `events`)
call the hub's HTTP-JSON API only, as the admin dashboard does, with a credential on this machine
([ADR-0031](adr/0031-the-cli-is-an-api-client-with-a-credential-on-this-machine.md); see
[Talking to a hub](#talking-to-a-hub)).

```sh
go install github.com/gravel-project/gravel/cmd/gravel@latest   # or a release archive, or make build
gravel hub help
```

## A hub on a cloud VM

`gravel hub create` makes three things, in this order, and finds them again by their tags and
names on every later run, so it keeps no state of its own:

| Resource | Name | What it is |
|---|---|---|
| Firewall | `gravel-hub-<name>` | SSH from the addresses you name, nothing else inbound; everything outbound. It applies to every instance tagged `gravel-hub:<name>` |
| Data volume | `<name>-data` | The runtime user's home: podman's storage (images, volumes, secrets), the units, the configuration. Formatted once, on its first boot |
| Instance | `<name>` | Fedora, booted from the cloud-init document (`gravel hub cloud-init` prints it), tagged `gravel-hub` and `gravel-hub:<name>` |

The instance has two users, both with your SSH key and no password: `admin`, with sudo, for the
OS, and `gravel`, the runtime user, with no sudo, whose rootless containers run the hub. Security
updates install themselves daily (dnf5-automatic, no reboot).

### DigitalOcean

1. A personal access token with **custom scopes**: `droplet`, `block_storage`,
   `block_storage_action`, `reserved_ip`, `reserved_ip_action`, `firewall` and `tag` (create,
   read, update, delete); `ssh_key` (create, read, delete); `vpc` (read); `project` (read,
   update: assigning a new resource to your project is an update); `actions`, `image`, `region`
   and `sizes` (read). A token reaches every project in its team: scopes limit what it may do,
   not where.
2. Save it where only you can read it; `gravel` refuses a token file others can read:

   ```sh
   install -m 600 /dev/stdin ~/.config/gravel/digitalocean-token    # paste, Enter, Ctrl-D
   export GRAVEL_DIGITALOCEAN_TOKEN_FILE=~/.config/gravel/digitalocean-token
   ```

3. Make the hub's host. Pick a region near what the hub talks to (your game servers, Discord), not
   near your members: they reach the hub through Cloudflare's edge.

   ```sh
   gravel hub create --name hub-1 --region sfo3 --size s-2vcpu-4gb --volume-size 20 \
     --project "My community" --ssh-from-here --dry-run      # what it would make; exit 3
   gravel hub create --name hub-1 --region sfo3 --size s-2vcpu-4gb --volume-size 20 \
     --project "My community" --ssh-from-here
   ```

   It waits for SSH and for cloud-init, then prints the address. The host's key is trusted on
   first use and kept in gravel's own `known_hosts` (`~/.config/gravel/known_hosts`).

   `--project` takes a project's name or its id. **Prefer the id:** renaming a project in the
   control panel breaks a name, never an id. Read the ids from the API:

   ```sh
   curl -s -H "Authorization: Bearer $(cat "$GRAVEL_DIGITALOCEAN_TOKEN_FILE")" \
     https://api.digitalocean.com/v2/projects | jq -r '.projects[] | "\(.id)  \(.name)"'
   ```

   or from the error itself: a name that matches no project fails before anything is made,
   listing every project with its id and suggesting the one whose name differs only in case and
   punctuation (`did you mean "My Community" (7fd3841c-…)?`). It never uses that one for you.

### Profiles

- **`--profile host`** (the default) is the machine. Install the hub with your own tooling over
  SSH as `gravel@<address>` (Hidden Token Gaming's Ansible does). `--extra-packages golang,gh`
  adds packages your tooling needs.
- **`--profile stock`** is the machine plus gravel's own units (`deploy/quadlet`: the hub,
  Postgres and the network) and a `hub.yaml` with login off. The database password and URL and
  the stats pseudonym key are generated on the host at the volume's first boot and never leave it.

  ```sh
  gravel hub create --name hub-1 --region sfo3 --size s-2vcpu-4gb --ssh-from-here \
    --profile stock --public-url https://hub.example.com --org-name "My community" \
    --tunnel-token-file ~/.config/gravel/tunnel-token
  gravel hub status --name hub-1          # prints the owner-claim token while the hub is unowned
  ```

  Missed it, or it expired? Mint a fresh one on the host (ADR-0036):
  `gravel hub ssh --name hub-1 -- podman exec gravel-hub /ko-app/gravel-hub claim-token --config /etc/gravel/hub.yaml`.

  `--tunnel-token-file` (mode 0600) installs a Cloudflare Tunnel's token as the
  `gravel-tunnel-token` podman secret over SSH, on standard input, and starts `gravel-cloudflared`;
  route the tunnel to `http://gravel-hub:8080`. Then turn login on as `docs/hub.md` "Login" says,
  in `~/.config/gravel/hub.yaml` on the host. `--hub-image` runs another image than the one the
  stock unit pins. The units and `hub.yaml` are written once per volume, so your edits survive a
  replaced host.

### Looking after it

```sh
gravel hub status    --name hub-1                    # firewall, volume, instance, claim token
gravel hub ssh       --name hub-1 [--admin] [cmd…]   # a shell as gravel (or admin)
gravel hub allow-ssh --name hub-1 --ssh-from-here    # your address changed
gravel hub destroy   --name hub-1 --yes              # the instance only; the volume stays
gravel hub create    --name hub-1 --region sfo3 --size s-2vcpu-4gb   # a new instance on the same data
gravel hub destroy   --name hub-1 --yes --delete-volume --delete-firewall   # everything, for good
```

**Replacing the host** (a broken OS, a new size, a new image) is `destroy` then `create`: the new
instance mounts the same volume and the hub comes back with its database, secrets, units and
configuration. A new instance has a new outbound address; anything that admits the hub by address
(a game server's allow list) needs the new one.

`create` changes nothing on an existing volume (its size, its contents) and never resizes an
instance. `--dry-run` on `create`, `destroy` and `allow-ssh` exits 3 when it would change
something and 0 when it would not. `--tag` (repeatable) on `create` adds tags of your own to the
instance and the volume when they are made, beside gravel's; the firewall never gets them.

### Machine-readable output

`--output json` on `create`, `status`, `destroy` and `allow-ssh` prints **one JSON object on
stdout** once the flags parse, also when the command fails (then with `error` set, and the message
on stderr too); the lines a person watches (waiting for cloud-init, its warnings) go to stderr.
The exit codes are the same as without it. A program written in Go imports the
[`hubvm`](../hubvm) package instead, which these commands are built on.

```sh
gravel hub create --name hub-1 --region sfo3 --size s-2vcpu-4gb --ssh-from 203.0.113.9 \
  --tag my-tenant --output json | jq -r .instance.public_ipv4
```

| Field | In | What it is |
|---|---|---|
| `hub` | all | the hub's name |
| `dry_run`, `changed` | create, destroy, allow-ssh | a dry run; whether anything changed, or would |
| `steps[]` | create, destroy, allow-ssh | `kind` (`firewall`, `volume`, `instance`), `name`, `action` (`unchanged`, `created`, `updated`, `deleted`; in a dry run, what would be done), `id` (the provider's; empty for what a dry run would make), `detail` (for a person: don't parse it) |
| `firewall` | create, status | `id`, `name`, `ssh_from[]` (CIDRs); `null` when there is none |
| `volume` | create, status | `id`, `name`, `region`, `size_gib`, `attached_to[]` (instance ids); `null` when there is none |
| `instance` | create, status | `id`, `name`, `status` (`pending`, `running`, `stopped`, `unknown`), `region`, `size`, `image`, `public_ipv4`, `private_ipv4` (empty until assigned), `tags[]`; `null` when there is none |
| `recreated` | create | a new instance on a volume that existed: the hub came back with its data |
| `ready` | create | SSH answered and cloud-init finished; `false` with `--no-wait` or `--dry-run` |
| `tunnel_installed` | create | `--tunnel-token-file`'s token installed and `gravel-cloudflared` started |
| `claim` | status | an unowned stock hub's owner-claim token, `token` and `expires_at`, read from its log over SSH; `null` when owned, not read (`--no-ssh`, no running instance) or unreadable |
| `warnings[]` | status | what went wrong without failing the command (the log could not be read) |
| `volume_kept` | destroy | the data volume was left for a later `create` (no `--delete-volume`) |
| `error` | all | present only when the command failed (exit 1) |

The field names are a public surface (docs/releasing.md "Versions"): one is never renamed or
removed without a **Breaking:** release; new ones may appear, so ignore what you don't know.

## Talking to a hub

```sh
gravel login --local --hub https://hub.example.org     # asks for a username and password
gravel claim Q_iZ5HNSAmLF…                             # an unowned hub: become its owner
gravel whoami                                          # who you are there, and what you may do
gravel fleet                                           # every server's health; exit 1 when one is down
gravel servers list
gravel servers status htg-wardogs-1
gravel events --since 1h --kind server                 # or --follow, until Ctrl-C
gravel logout
```

**The hub** is `--hub`, else `GRAVEL_HUB`, else the one you last logged in to. A bare host name
means `https://`.

**Logging in.** `gravel login --local` logs in with a local account
([ADR-0026](adr/0026-local-accounts-a-username-and-password-are-a-provider.md)): it asks for the
username (or takes `--username`) and the password without echo, or reads them a line each from
stdin when that is not a terminal, and posts them to the hub's own login form, so the hub's lockout
and its limit per address apply as they do in a browser. It keeps the session cookie the hub sets.
An account made with `gravel-hub users create-local` gets its password from the one-time link the
command prints, in a browser. Over plain HTTP to another machine it warns: the password and the
session cross the network in the clear.

`gravel login --app --client-id ID --secret-file FILE` calls the hub as a registered app instead
(`gravel-hub apps create`), within the app's scopes: the secret stays in its file (mode 0600, or it
is refused) and the CLI trades it for a token on each run. `claim` refuses an app: an owner is a
member.

**The credential** is `~/.config/gravel/credentials` (`GRAVEL_CREDENTIALS` names another), mode
0600 in a 0700 directory, one entry per hub; a file others can read is refused, as a session cookie
in it is as good as the password:

```json
{
  "current": "https://hub.example.org",
  "credentials": [
    {"hub_url": "https://hub.example.org", "kind": "session", "cookie": "__Host-gravel_session=…"},
    {"hub_url": "https://hub-2.example.org", "kind": "app", "client_id": "gravel_47ca…", "secret_file": "/home/me/.config/gravel/hub-2-secret"}
  ]
}
```

`gravel logout` ends the session on the hub and forgets the entry. A session ends on the hub too
(its lifetime, "log out everywhere", staff revoking it); the next command then exits 4 and says to
log in again.

**Events.** `gravel events` prints the newest 20 (`--limit`), or every one since `--since` (a
duration back, `1h`, or a time, `2026-10-14T02:00:00Z`), oldest first, in UTC. `--kind` (a kind,
`server.down`, or a group, `server`; repeatable), `--severity` (that and above) and `--server`
narrow it. `--follow` then asks for what is new every 5 seconds, from the log's head when it
started, until interrupted; a failed poll is reported and retried. It needs `events:read`.

**The wire.** Every call is Connect's protocol with JSON bodies (connect-go with `WithProtoJSON`):
what `curl` sends, and what a proxy can read. The gRPC stack is not in the binary; a test on its
dependencies keeps it out.

## Operating a hub

After bootstrap (the hub is up, someone claimed it), an operator runs it from their own machine:
these commands call the same procedures as the admin dashboard
([docs/hub.md](hub.md#admin-procedures-adr-0031)), and the hub checks each against what the
logged-in member holds. Where `gravel-hub` has the command on the host they take its flags and
print what it prints, so a script moves from one to the other by changing the binary.

```sh
gravel members list --game cs2                         # members, their roles and identities
gravel members grant discord:1234 operator --game cs2  # a member by id, or by an identity
gravel members revoke local:sam moderator
gravel members logout local:sam                        # end every session they have
gravel users create-local --username sam               # prints the one-time password link
gravel apps create --name "HTG Bot" --scopes identity:read,roles:grant
gravel settings export > organization.yaml
gravel settings apply organization.yaml --dry-run      # exit 3: the sections that would change
gravel servers apply servers.yaml                      # all but what only the host changes
gravel stats erase --provider steam --subject 7656119…
gravel notifiers test john-phone
gravel owner nominate local:sam                        # then Sam runs: gravel owner accept
```

**Members** are named by their id (`gravel members list` shows it) or by one of their identities
as `provider:subject`: `local:sam`, `discord:1234`, `steam:7656119…`. `members list` needs
`identity:read`; `grant` and `revoke` (admin, operator or moderator, instance-wide or for one
game with `--game`) need `roles:grant`; `logout` needs `members:manage`.

**Manifests.** `settings` and `servers` `export` print the manifest, `check` validates one
against this hub, and `apply` stores it; `-` reads it from stdin. The hub parses and checks it with
its own rules and names every problem. `apply --dry-run` says what would change and exits 3 when
something would, so a converge can check before it writes; applying a manifest that matches what
is stored changes nothing and records no event. A servers manifest that adds a location or a
server, or changes a file or where a secret goes, is refused: that is `gravel-hub servers apply`
on the host ([docs/hub.md](hub.md#admin-procedures-adr-0031)).

**In person.** These are a member's: a session from `gravel login --local`. An app's credential is
refused, as an app may not hold these capabilities. `owner nominate` is the owner's; `owner
accept` the nominee's, who may also decline with `owner cancel`.

What stays on the host: `gravel-hub serve`, `migrate` and the other commands
[docs/hub.md](hub.md#commands) lists as host-only, the offline `settings check` and
`servers check` for CI, and `users create-local` for the first member, before anyone can log in.

## A hub host in a local VM

`deploy/hub-vm.sh` boots the same document on Fedora Cloud under qemu/KVM with a second disk as the
data volume, so you can try a profile, or rehearse a move, without a cloud account:

```sh
deploy/hub-vm.sh up --profile host        # or stock; SSH on 127.0.0.1:2222
deploy/hub-vm.sh check                    # the document finished; stock: the hub answers /readyz
deploy/hub-vm.sh ssh [--admin] [cmd…]
deploy/hub-vm.sh recreate                 # a new instance on the same data disk
deploy/hub-vm.sh down [--wipe]
make hub-vm                               # the CI drill: stock from nothing, recreate, compare
```

It needs `qemu-system-x86_64`, a writable `/dev/kvm`, `genisoimage` (or `xorriso`), `ssh` and Go.

The VMs a hub makes for its game servers are the hub's, not the CLI's: they come from `servers.yaml`
(`locations` and a server with `driver: cloudvm`) and boot from the same kind of document, and
`deploy/game-vm.sh` rehearses one locally ([docs/hub.md](hub.md#cloud-vm-servers-adr-0003)).

## Commands

| Command | What it does | Exit codes |
|---|---|---|
| `gravel hub create` | make or find the firewall, volume and instance; wait for SSH and cloud-init; `--tag`; `--output json` | 0 done; 1 failed; 2 usage; 3 `--dry-run` would change something |
| `gravel hub status` | what exists; the owner-claim token of an unowned stock hub (over SSH; `--no-ssh` skips it); `--output json` | 0; 1 |
| `gravel hub destroy` | delete the instance; `--delete-volume`, `--delete-firewall`; needs `--yes`; `--output json` | 0; 1; 2; 3 `--dry-run` |
| `gravel hub allow-ssh` | replace the addresses SSH is admitted from; `--output json` | 0; 1; 2; 3 `--dry-run` |
| `gravel hub ssh` | ssh to the host as `gravel` (`--admin`: as `admin`) | the remote command's |
| `gravel hub cloud-init` | print the document for `--name` and `--device` | 0; 1; 2 |
| `gravel login` | `--local`: a local account's session; `--app`: an app's client id and secret file | 0; 1 refused; 2 usage |
| `gravel logout` | end the session on the hub and forget the credential | 0; 1 |
| `gravel whoami` | the member (their roles, identities and capabilities), or the app | 0; 1; 4 log in |
| `gravel claim TOKEN` | become the owner of an unowned hub (ADR-0002) | 0; 1 refused or owned; 2; 4 |
| `gravel fleet` | the totals, the open alarms and each server's health, trouble first | 0; 1 a server is down, or failed; 4 |
| `gravel servers list` | every server: game, location, driver, state, players, map | 0; 1; 4 |
| `gravel servers status SERVER` | one server's last observation | 0; 1 unknown or failed; 4 |
| `gravel events` | the events log; `--follow`, `--since`, `--kind`, `--severity`, `--server`, `--limit` | 0; 1; 2; 4 |
| `gravel servers export` | the games and servers as a `servers.yaml` manifest | 0; 1; 4 |
| `gravel servers check MANIFEST` | validate a manifest against the hub | 0 valid; 1 every problem named; 2; 4 |
| `gravel servers apply MANIFEST` | apply it, all but what only the host changes; prints each change | 0; 1 refused; 2; 3 `--dry-run` would change something; 4 |
| `gravel members list` | the members: id, name, roles, identities, last login; `--game` | 0; 1; 4 |
| `gravel members grant\|revoke MEMBER ROLE` | grant or revoke admin, operator or moderator; `--game` for one game | 0; 1 not a member or refused; 2; 4 |
| `gravel members logout MEMBER` | end every session the member has | 0; 1; 2; 4 |
| `gravel owner status` | who owns the hub, and the nomination | 0; 1; 4 |
| `gravel owner nominate MEMBER` | name the member who may take the hub over, for 7 days | 0; 1; 2; 4 |
| `gravel owner accept\|cancel` | the nominee takes the hub over; the owner cancels or the nominee declines | 0; 1 no nomination; 4 |
| `gravel users create-local\|reset-password` | a local account or a reset, and its one-time password link; `--username`, `--email` | 0; 1; 2; 4 |
| `gravel apps create\|list\|revoke` | register an app (its secret once), list them, revoke one; `gravel-hub apps`' flags | 0; 1; 2; 4 |
| `gravel settings export` | the Organization settings as a manifest | 0; 1; 4 |
| `gravel settings check MANIFEST` | validate a manifest against the hub | 0 valid; 1 every problem named; 2; 4 |
| `gravel settings apply MANIFEST` | store it; says which sections changed | 0; 1; 2; 3 `--dry-run` would change something; 4 |
| `gravel stats erase` | erase a player's identity from the stats; `--provider`, `--subject` | 0; 1; 2; 4 |
| `gravel export -o FILE` | everything the hub holds as an archive (docs/hub.md, Export and import), streamed; the owner, in person; FILE written 0600 and never overwritten, `-` for stdout | 0; 1; 2; 4 |
| `gravel notifiers test NAME` | send the notifier a test event | 0 delivered; 1 not, or no such notifier; 4 |
| `gravel version` | the version | 0 |

Exit 3 is a `--dry-run` that would change something, as with `gravel hub` and `gravel-hub`. Exit 4
from a command that calls a hub means log in (again): no credential for the hub, a session that
ended, or an app's credentials the hub refused. Every hub command takes `--hub`.

Every cloud command takes `--provider` (`digitalocean`), `--token-file` (default
`$GRAVEL_<PROVIDER>_TOKEN_FILE`) and `--project`.
