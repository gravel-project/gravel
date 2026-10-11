#!/usr/bin/env bash
# A hub host in a local VM (ADR-0020): Fedora Cloud booted under qemu/KVM from the same cloud-init
# document `gravel hub create` gives a cloud instance, with a second disk as the data volume. It
# proves the document without a cloud account; CI runs it (the hub-vm job) and an operator can
# rehearse a move on it.
#
#   deploy/hub-vm.sh up       [--profile host|stock] [--ssh-key FILE] [--extra-packages a,b]
#   deploy/hub-vm.sh check    the document finished; stock: the hub answers /readyz
#   deploy/hub-vm.sh recreate a new instance on the same data disk (the root disk is thrown away)
#   deploy/hub-vm.sh ssh      [--admin] [command...]
#   deploy/hub-vm.sh down     stop the VM (the disks stay); `down --wipe` deletes them too
#   deploy/hub-vm.sh drill    the CI drill: a stock hub from nothing, checked; a new instance on the
#                             same data disk, checked; the same database and secrets came back
#
# State lives in $HUB_VM_DIR (default .hub-vm/ in the repository); the Fedora image is cached in
# $HUB_VM_CACHE (default ~/.cache/gravel/hub-vm). SSH reaches the VM on 127.0.0.1:$HUB_VM_SSH_PORT
# (default 2222). Needs qemu-system-x86_64, /dev/kvm, genisoimage (or xorriso), curl, sha256sum, ssh
# and Go (it renders the document with `go run ./cmd/gravel`).
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
dir="${HUB_VM_DIR:-$root/.hub-vm}"
cache="${HUB_VM_CACHE:-$HOME/.cache/gravel/hub-vm}"
port="${HUB_VM_SSH_PORT:-2222}"
fedora_release=44
fedora_compose=1.7
image_name="Fedora-Cloud-Base-Generic-${fedora_release}-${fedora_compose}.x86_64.qcow2"
image_base="https://download.fedoraproject.org/pub/fedora/linux/releases/${fedora_release}/Cloud/x86_64/images"
checksum_name="Fedora-Cloud-${fedora_release}-${fedora_compose}-x86_64-CHECKSUM"
name=hub-vm
device=/dev/disk/by-id/virtio-gravel-data

log() { printf 'hub-vm: %s\n' "$*" >&2; }
die() { log "$*"; exit 1; }

ssh_opts=(-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR
  -o ConnectTimeout=5 -o BatchMode=yes -p "$port")
vm_ssh() { local user="$1"; shift; ssh "${ssh_opts[@]}" "$user@127.0.0.1" "$@"; }

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

seed() {
  # A new instance-id makes cloud-init treat the boot as a new instance: the per-instance modules
  # (users, runcmd, first-boot) run again, as on a replacement cloud instance.
  local iid="hub-vm-$(date +%s)-$RANDOM"
  mkdir -p "$dir/seed"
  # No local-hostname: cloud-init would set it in its local stage, before the system bus is up on
  # Fedora, and mark the run degraded; the document's runcmd sets it instead.
  printf 'instance-id: %s\n' "$iid" > "$dir/seed/meta-data"
  (cd "$root" && go run ./cmd/gravel hub cloud-init --name "$name" --device "$device" "${doc_args[@]}") > "$dir/seed/user-data"
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
  rm -f "$dir/root.qcow2"
  qemu-img create -q -f qcow2 -F qcow2 -b "$img" "$dir/root.qcow2" 20G
  [[ -f "$dir/data.qcow2" ]] || qemu-img create -q -f qcow2 "$dir/data.qcow2" 10G
  seed
  qemu-system-x86_64 -enable-kvm -cpu host -machine q35 -smp 2 -m 3072 \
    -drive "file=$dir/root.qcow2,if=none,id=root,format=qcow2" -device virtio-blk-pci,drive=root,bootindex=0 \
    -drive "file=$dir/data.qcow2,if=none,id=data,format=qcow2" -device virtio-blk-pci,drive=data,serial=gravel-data \
    -drive "file=$dir/seed.iso,media=cdrom,readonly=on" \
    -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$port-:22" -device virtio-net-pci,netdev=n0 \
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
  # status --wait exits 0 when done, 1 on a critical error, 2 on a recoverable one (a module failed).
  set +e
  vm_ssh admin sudo cloud-init status --wait --long
  rc=$?
  set -e
  if (( rc != 0 )); then
    vm_ssh admin sudo cat /var/log/cloud-init-output.log | tail -60 >&2 || true
    die "cloud-init reported errors (exit $rc)"
  fi
  log "ready: ssh -p $port gravel@127.0.0.1 (or admin@)"
}

stop() {
  if [[ -f "$dir/qemu.pid" ]] && kill -0 "$(cat "$dir/qemu.pid" 2>/dev/null)" 2>/dev/null; then
    local pid; pid="$(cat "$dir/qemu.pid" 2>/dev/null || true)"
    vm_ssh admin sudo systemctl poweroff 2>/dev/null || true
    # qemu deletes its pidfile when it exits, so the pid was read once, above.
    for _ in $(seq 1 30); do [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null || break; sleep 1; done
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  fi
  rm -f "$dir/qemu.pid"
}

check() {
  # 0 is done with no error and no warning; 2 is done but degraded (a module warned).
  vm_ssh admin sudo cloud-init status >/dev/null || die "cloud-init is not done, or finished degraded (cloud-init status --long)"
  [[ "$(vm_ssh admin hostname)" == "$name" ]] || die "the hostname is not $name"
  vm_ssh gravel 'mountpoint -q "$HOME" && test -e "$HOME/.gravel-home"' || die "the data volume is not the runtime user's home"
  [[ "$(vm_ssh gravel id -u)" == "1100" ]] || die "the runtime user's uid is not 1100"
  vm_ssh gravel 'grep -q "^gravel:524288:65536$" /etc/subuid' || die "the runtime user's subordinate ids are not fixed"
  vm_ssh gravel 'loginctl show-user gravel -p Linger --value | grep -qx yes' || die "linger is off"
  vm_ssh admin 'systemctl is-enabled dnf5-automatic.timer >/dev/null 2>&1 || systemctl is-enabled dnf-automatic.timer >/dev/null' || die "automatic security updates are off"
  if vm_ssh gravel 'test -e "$HOME/.gravel-stock"'; then
    for _ in $(seq 1 60); do
      vm_ssh gravel 'curl -fsS http://127.0.0.1:8080/readyz' >/dev/null 2>&1 && break
      sleep 5
    done
    vm_ssh gravel 'curl -fsS http://127.0.0.1:8080/readyz' || die "the stock hub does not answer /readyz"
    vm_ssh gravel 'podman secret ls --format "{{.Name}}" | sort | tr "\n" " "'
    echo
  fi
  log "check passed"
}

# fingerprint is what must survive a new instance on the same data disk: the secrets' ids, the
# Postgres volume, and the organization row the hub made.
fingerprint() {
  vm_ssh gravel 'podman secret ls --format "{{.ID}} {{.Name}}" | sort
    podman volume inspect systemd-gravel-postgres --format "{{.CreatedAt}}"
    podman exec gravel-postgres psql -U gravel -d gravel -tAc "select id, created_at from organizations"'
}

drill() {
  doc_args=(--profile stock --public-url http://127.0.0.1:8080)
  [[ -n "${HUB_VM_SSH_KEY:-}" ]] && doc_args+=(--ssh-key "$HUB_VM_SSH_KEY")
  printf '%s\n' "${doc_args[@]}" > "$dir/doc-args"
  local started=$SECONDS before after
  boot
  check
  log "first boot to a ready hub: $((SECONDS - started)) s"
  before="$(fingerprint)"
  [[ -n "$before" ]] || die "nothing to compare: no secrets, volume or organization"
  started=$SECONDS
  stop
  boot
  check
  log "recreate to a ready hub: $((SECONDS - started)) s"
  after="$(fingerprint)"
  if [[ "$before" != "$after" ]]; then
    printf 'before:\n%s\nafter:\n%s\n' "$before" "$after" >&2
    die "the new instance did not get the same data back"
  fi
  log "drill passed: the same secrets, volume and organization after a new instance"
}

cmd="${1:-}"; shift || true
doc_args=()
admin=false
wipe=false
while (( $# )); do
  case "$1" in
    --profile|--ssh-key|--extra-packages|--public-url|--hub-image|--org-name) doc_args+=("$1" "$2"); shift 2 ;;
    --admin) admin=true; shift ;;
    --wipe) wipe=true; shift ;;
    --) shift; break ;;
    *) [[ "$cmd" == ssh ]] && break; die "unknown flag $1" ;;
  esac
done
if [[ " ${doc_args[*]} " == *" --profile stock "* && " ${doc_args[*]} " != *" --public-url "* ]]; then
  doc_args+=(--public-url "http://127.0.0.1:8080")
fi
mkdir -p "$dir"

case "$cmd" in
  up)
    [[ -f "$dir/qemu.pid" ]] && kill -0 "$(cat "$dir/qemu.pid")" 2>/dev/null && die "already running (down first)"
    printf '%s\n' "${doc_args[@]+"${doc_args[@]}"}" > "$dir/doc-args"
    boot ;;
  recreate)
    [[ -f "$dir/doc-args" ]] && mapfile -t doc_args < "$dir/doc-args"
    stop
    boot ;;
  check) check ;;
  drill)
    [[ -f "$dir/qemu.pid" ]] && kill -0 "$(cat "$dir/qemu.pid")" 2>/dev/null && die "already running (down first)"
    rm -f "$dir/data.qcow2"
    trap stop EXIT
    drill ;;
  ssh) if $admin; then vm_ssh admin "$@"; else vm_ssh gravel "$@"; fi ;;
  down)
    stop
    if $wipe; then rm -rf "$dir"; fi ;;
  *) sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
esac
