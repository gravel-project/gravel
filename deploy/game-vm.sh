#!/usr/bin/env bash
# A cloud-VM game host in a local VM (ADR-0003): Fedora Cloud under qemu/KVM, booted from the
# cloud-init document the hub gives a cloud instance for a Counter-Strike 2 server, with a second
# disk as the data volume. It proves on a developer's machine what the cloud-VM driver does to a
# cloud VM, through the driver's own code (go run ./internal/tools/gamevm): the document, the
# environment file pushed over SSH, the game in its rootless container with the image's user
# mapped onto the runtime user, RCON, and the log push with its redaction and roster.
#
#   deploy/game-vm.sh up         boot the host from the document
#   deploy/game-vm.sh check      the document finished; the game's unit is installed and waiting
#   deploy/game-vm.sh push       push the environment file over SSH; the unit starts the game
#   deploy/game-vm.sh started    the container runs as the mapped user and SteamCMD is updating
#   deploy/game-vm.sh watch      wait for the game to answer RCON (a first start downloads ~60 GB),
#                                then for a log batch on the local log route
#   deploy/game-vm.sh recreate   a new instance on the same data disk (the root disk is thrown away)
#   deploy/game-vm.sh ssh        [--admin] [command...]
#   deploy/game-vm.sh down       stop the VM (the disks stay); `down --wipe` deletes them too
#   deploy/game-vm.sh drill      up, check, push, started, down: everything short of the download
#   deploy/game-vm.sh drill-full drill plus watch, then a new instance that starts the game alone
#
# State lives in $GAME_VM_DIR (default .game-vm/); the Fedora image is shared with hub-vm.sh in
# $GAME_VM_CACHE (default ~/.cache/gravel/hub-vm). SSH reaches the VM on 127.0.0.1:$GAME_VM_SSH_PORT
# (2223), the game's port 27015 (TCP for RCON, UDP to play) on 127.0.0.1:$GAME_VM_GAME_PORT (27115),
# and the VM reaches the log route on the host at 10.0.2.2:$GAME_VM_LOG_PORT (18091). The data disk
# is a 100 GiB sparse file; a full install fills about 60 GiB of it.
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
dir="${GAME_VM_DIR:-$root/.game-vm}"
cache="${GAME_VM_CACHE:-$HOME/.cache/gravel/hub-vm}"
port="${GAME_VM_SSH_PORT:-2223}"
game_port="${GAME_VM_GAME_PORT:-27115}"
log_port="${GAME_VM_LOG_PORT:-18091}"
fedora_release=44
fedora_compose=1.7
image_name="Fedora-Cloud-Base-Generic-${fedora_release}-${fedora_compose}.x86_64.qcow2"
image_base="https://download.fedoraproject.org/pub/fedora/linux/releases/${fedora_release}/Cloud/x86_64/images"
checksum_name="Fedora-Cloud-${fedora_release}-${fedora_compose}-x86_64-CHECKSUM"
name=game-vm
device=/dev/disk/by-id/virtio-gravel-data

log() { printf 'game-vm: %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }

key="$dir/hub-key"
ssh_opts=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
  -o ConnectTimeout=5 -o BatchMode=yes -o IdentitiesOnly=yes -i "$key" -p "$port")
vm_ssh() { local user="$1"; shift; ssh "${ssh_opts[@]}" "$user@127.0.0.1" "$@"; }
tool() { (cd "$root" && go run ./internal/tools/gamevm "$@"); }

image() {
  mkdir -p "$cache"
  local img="$cache/$image_name"
  if [[ ! -f "$img" ]]; then
    log "downloading $image_name"
    curl -fsSL -o "$cache/$checksum_name" "$image_base/$checksum_name"
    curl -fsSL -o "$img.part" "$image_base/$image_name"
    want="$(grep -E "^SHA256 \($image_name\)" "$cache/$checksum_name" | awk '{print $NF}')"
    [[ "$want" =~ ^[0-9a-f]{64}$ ]] || die "no SHA256 for $image_name in $checksum_name"
    got="$(sha256sum "$img.part" | awk '{print $1}')"
    [[ "$got" == "$want" ]] || { rm -f "$img.part"; die "checksum mismatch for $image_name"; }
    mv "$img.part" "$img"
  fi
  printf '%s\n' "$img"
}

# secrets are the drill's own: the hub's SSH key, the RCON password and the feed token.
secrets() {
  mkdir -p "$dir"
  [[ -f "$key" ]] || ssh-keygen -q -t ed25519 -N '' -C 'gravel game-vm drill' -f "$key"
  [[ -f "$dir/rcon" ]] || od -An -tx1 -N16 /dev/urandom | tr -d ' \n' > "$dir/rcon"
  [[ -f "$dir/token" ]] || od -An -tx1 -N16 /dev/urandom | tr -d ' \n' > "$dir/token"
  chmod 0600 "$key" "$dir/rcon" "$dir/token"
}

seed() {
  local iid
  iid="game-vm-$(date +%s)-$RANDOM"
  mkdir -p "$dir/seed"
  printf 'instance-id: %s\n' "$iid" > "$dir/seed/meta-data"
  tool document --device "$device" --ssh-key "$key.pub" --name "$name" > "$dir/seed/user-data"
  rm -f "$dir/seed.iso"
  if command -v genisoimage >/dev/null; then
    genisoimage -quiet -output "$dir/seed.iso" -volid cidata -joliet -rock "$dir/seed/user-data" "$dir/seed/meta-data"
  else
    xorriso -as genisoimage -quiet -output "$dir/seed.iso" -volid cidata -joliet -rock "$dir/seed/user-data" "$dir/seed/meta-data"
  fi
}

boot() {
  local img; img="$(image)"
  [[ -w /dev/kvm ]] || die "/dev/kvm is not writable (KVM is required)"
  secrets
  rm -f "$dir/root.qcow2"
  qemu-img create -q -f qcow2 -F qcow2 -b "$img" "$dir/root.qcow2" 20G
  [[ -f "$dir/data.qcow2" ]] || qemu-img create -q -f qcow2 "$dir/data.qcow2" 100G
  seed
  qemu-system-x86_64 -enable-kvm -cpu host -machine q35 -smp 2 -m 4096 \
    -drive "file=$dir/root.qcow2,if=none,id=root,format=qcow2" -device virtio-blk-pci,drive=root,bootindex=0 \
    -drive "file=$dir/data.qcow2,if=none,id=data,format=qcow2" -device virtio-blk-pci,drive=data,serial=gravel-data \
    -drive "file=$dir/seed.iso,media=cdrom,readonly=on" \
    -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$port-:22,hostfwd=tcp:127.0.0.1:$game_port-:27015,hostfwd=udp:127.0.0.1:$game_port-:27015" \
    -device virtio-net-pci,netdev=n0 \
    -display none -serial "file:$dir/console.log" \
    -daemonize -pidfile "$dir/qemu.pid"
  log "booting (console: $dir/console.log)"
  wait_ready
}

wait_ready() {
  for _ in $(seq 1 120); do
    vm_ssh admin true 2>/dev/null && break
    sleep 5
  done
  vm_ssh admin true || die "no SSH after 10 minutes; see $dir/console.log"
  set +e
  vm_ssh admin sudo cloud-init status --wait --long
  rc=$?
  set -e
  if (( rc != 0 )); then
    vm_ssh admin sudo cat /var/log/cloud-init-output.log | tail -60 >&2 || true
    die "cloud-init reported errors (exit $rc)"
  fi
  log "ready: deploy/game-vm.sh ssh (as gravel) or ssh --admin"
}

stop() {
  if [[ -f "$dir/qemu.pid" ]] && kill -0 "$(cat "$dir/qemu.pid" 2>/dev/null)" 2>/dev/null; then
    local pid; pid="$(cat "$dir/qemu.pid" 2>/dev/null || true)"
    vm_ssh admin sudo systemctl poweroff 2>/dev/null || true
    for _ in $(seq 1 60); do [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null || break; sleep 1; done
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  fi
  rm -f "$dir/qemu.pid"
}

user_systemctl() { vm_ssh gravel "XDG_RUNTIME_DIR=/run/user/1100 systemctl --user $*"; }

check() {
  vm_ssh admin sudo cloud-init status >/dev/null || die "cloud-init is not done, or finished degraded"
  [[ "$(vm_ssh admin hostname)" == "$name" ]] || die "the hostname is not $name"
  vm_ssh gravel 'mountpoint -q "$HOME" && test -e "$HOME/.gravel-home" && test -e /var/lib/gravel/boot-done' ||
    die "the data volume is not the runtime user's home, or first boot did not finish"
  [[ "$(vm_ssh gravel id -u)" == "1100" ]] || die "the runtime user's uid is not 1100"
  vm_ssh gravel 'test -f "$HOME/.config/containers/systemd/cs2.container"' || die "the game's unit is not installed"
  user_systemctl cat cs2.service >/dev/null || die "quadlet made no cs2.service"
  if ! vm_ssh gravel 'test -e "$HOME/.config/cs2/env"'; then
    [[ "$(user_systemctl show cs2.service -p ConditionResult --value)" == "no" ]] ||
      die "the game's unit did not wait for its environment file"
    log "check passed: the unit is installed and waits for its environment file"
  else
    log "check passed: the environment file is on the volume"
  fi
}

push() {
  secrets
  tool push --ssh "127.0.0.1:$port" --key "$key" --rcon-password "$(cat "$dir/rcon")" \
    --feed "http://10.0.2.2:$log_port/api/ingest/log/$(cat "$dir/token")"
}

# started waits for the container (the image pull is a few hundred MB) and SteamCMD's first lines,
# and checks the image's steam user (uid 1000) owns the install as the VM's runtime user.
started() {
  for _ in $(seq 1 120); do
    vm_ssh gravel 'podman ps --filter name=^cs2$ --format "{{.Names}}"' 2>/dev/null | grep -qx cs2 && break
    sleep 5
  done
  vm_ssh gravel 'podman ps --filter name=^cs2$ --format "{{.Names}} {{.Status}}"' | grep -q '^cs2 Up' ||
    { user_systemctl status cs2.service --no-pager | tail -20 >&2; die "the game's container is not running"; }
  [[ "$(vm_ssh gravel 'podman exec cs2 id -u')" == "1000" ]] || die "the game does not run as the image's user (1000)"
  [[ "$(vm_ssh gravel 'stat -c %U "$HOME/cs2"')" == "gravel" ]] || die "the install is not the runtime user's on the volume"
  for _ in $(seq 1 60); do
    vm_ssh gravel 'podman logs cs2 2>&1 | grep -qiE "steamcmd|update state|Redirecting stderr|Success"' && break
    sleep 5
  done
  vm_ssh gravel 'podman logs cs2 2>&1 | tail -5'
  log "started: the container runs as uid 1000 (the VM's gravel), SteamCMD is at work"
}

watch() {
  secrets
  tool watch --rcon "127.0.0.1:$game_port" --rcon-password "$(cat "$dir/rcon")" \
    --listen "127.0.0.1:$log_port" --token "$(cat "$dir/token")" --timeout "${GAME_VM_TIMEOUT:-90m}"
}

drill() {
  local t=$SECONDS
  boot; check
  log "first boot: $((SECONDS - t)) s"
  t=$SECONDS
  push; started
  log "env pushed to SteamCMD at work: $((SECONDS - t)) s"
}

cmd="${1:-}"; shift || true
admin=false
wipe=false
while (( $# )); do
  case "$1" in
    --admin) admin=true; shift ;;
    --wipe) wipe=true; shift ;;
    --) shift; break ;;
    *) [[ "$cmd" == ssh ]] && break; die "unknown flag $1" ;;
  esac
done
mkdir -p "$dir"

case "$cmd" in
  up)
    [[ -f "$dir/qemu.pid" ]] && kill -0 "$(cat "$dir/qemu.pid")" 2>/dev/null && die "already running (down first)"
    boot ;;
  check) check ;;
  push) push ;;
  started) started ;;
  watch) watch ;;
  recreate) stop; boot ;;
  ssh)
    if $admin; then vm_ssh admin "$@"; else vm_ssh gravel "$@"; fi ;;
  down)
    stop
    if $wipe; then rm -rf "$dir"; log "wiped"; fi ;;
  drill)
    drill
    stop
    log "drill passed" ;;
  drill-full)
    drill
    t=$SECONDS
    watch
    log "the game answered and pushed its log: $((SECONDS - t)) s after it started"
    t=$SECONDS
    stop; boot; check
    # The environment file and the install are on the volume: the new instance starts the game
    # alone, with no push, and no download.
    GAME_VM_TIMEOUT=20m watch
    log "a new instance on the same disk played again in $((SECONDS - t)) s"
    stop
    log "full drill passed" ;;
  *) die "usage: $0 up|check|push|started|watch|recreate|ssh|down|drill|drill-full" ;;
esac
