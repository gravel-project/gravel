# The gravel CLI

`gravel` is the operator's command line. Its first commands make and look after the hub's own
host through a cloud provider ([ADR-0020](adr/0020-the-hubs-own-host-made-by-the-cli-through-the-cloud-provider.md)).
Commands that act on a running hub (`gravel claim` and the rest of build order step 5) will call
the hub's HTTP-JSON API only.

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
something and 0 when it would not.

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
| `gravel hub create` | make or find the firewall, volume and instance; wait for SSH and cloud-init | 0 done; 1 failed; 2 usage; 3 `--dry-run` would change something |
| `gravel hub status` | what exists; the owner-claim token of an unowned stock hub (over SSH; `--no-ssh` skips it) | 0; 1 |
| `gravel hub destroy` | delete the instance; `--delete-volume`, `--delete-firewall`; needs `--yes` | 0; 1; 2; 3 `--dry-run` |
| `gravel hub allow-ssh` | replace the addresses SSH is admitted from | 0; 1; 2; 3 `--dry-run` |
| `gravel hub ssh` | ssh to the host as `gravel` (`--admin`: as `admin`) | the remote command's |
| `gravel hub cloud-init` | print the document for `--name` and `--device` | 0; 1; 2 |
| `gravel version` | the version | 0 |

Every cloud command takes `--provider` (`digitalocean`), `--token-file` (default
`$GRAVEL_<PROVIDER>_TOKEN_FILE`) and `--project`.
