#!/usr/bin/env bash
# Node-side checks for Dell SC Series FC volumes.
#
#   sc-node.sh inventory          read-only: HBAs, SC target ports, SC multipath maps, foreign LUNs
#   sc-node.sh rescan             scan FC HBAs for new LUNs (adds devices only, never removes)
#   sc-node.sh show <wwid>        show one multipath map, e.g. 36000d31000fa0c000000000000000abc
#   sc-node.sh release <wwid>     flush the map and delete its SCSI paths (run BEFORE unmapping on the array)
#
# 'release' refuses to touch a map that is open, larger than MAX_GIB (default 64), not COMPELNT,
# or carries any filesystem/partition signature. It is meant only for disposable probe volumes.
set -euo pipefail

SC_OUI="5000d31"           # Compellent target/NAA prefix
MAX_GIB="${MAX_GIB:-64}"

need_root() { [ "$(id -u)" -eq 0 ] || exec sudo "$0" "$@"; }

inventory() {
  echo "### Host"
  echo "hostname: $(hostname)   kernel: $(uname -r)   $(. /etc/os-release; echo "$PRETTY_NAME")"
  echo "multipathd: $(systemctl is-active multipathd 2>/dev/null || true)"
  echo
  echo "### FC HBAs (initiator WWPNs -> register these on the SC server object)"
  for h in /sys/class/fc_host/host*; do
    [ -e "$h" ] || { echo "no FC HBAs"; break; }
    printf "%-8s wwpn=%s state=%s speed=%s fabric=%s model=%s\n" "$(basename "$h")" \
      "$(cat "$h/port_name")" "$(cat "$h/port_state")" "$(cat "$h/speed")" \
      "$(cat "$h/fabric_name")" "$(cut -d' ' -f1 "$h/symbolic_name" 2>/dev/null)"
  done
  echo
  echo "### FC target ports seen (SC = 0x${SC_OUI}...)"
  for r in /sys/class/fc_remote_ports/rport-*; do
    [ -e "$r" ] || continue
    grep -q Target "$r/roles" || continue
    wwpn=$(cat "$r/port_name"); kind=other
    [[ "$wwpn" == 0x${SC_OUI}* ]] && kind=SC
    printf "%-14s %s %-5s %s\n" "$(basename "$r")" "$wwpn" "$kind" "$(cat "$r/port_state")"
  done
  echo
  echo "### Multipath maps"
  printf "%-10s %-36s %-9s %-10s %-5s %s\n" MAP WWID VENDOR SIZE OPEN SIGNATURE
  dmsetup ls --target multipath 2>/dev/null | awk '{print $1}' | sort | while read -r m; do
    wwid=$(dmsetup info -c --noheadings -o uuid "$m" | sed 's/^mpath-//')
    open=$(dmsetup info -c --noheadings -o open "$m" | tr -d ' ')
    dev=/dev/mapper/$m
    slave=$(ls "/sys/block/$(basename "$(readlink -f "$dev")")/slaves" | head -1)
    vendor=$(tr -d ' ' < "/sys/block/$slave/device/vendor" 2>/dev/null || echo "?")
    size=$(lsblk -dno SIZE "$dev" | tr -d ' ')
    sig=$(blkid -p -o value -s TYPE "$dev" 2>/dev/null || true)
    [ -z "$sig" ] && [ -e "${dev}-part1" ] && sig="part1:$(blkid -p -o value -s TYPE "${dev}-part1" 2>/dev/null || echo '?')"
    [ -z "$sig" ] && sig=$(blkid -p -o value -s PTTYPE "$dev" 2>/dev/null || true)
    printf "%-10s %-36s %-9s %-10s %-5s %s\n" "$m" "$wwid" "$vendor" "$size" "$open" "${sig:--}"
  done
  echo
  echo "Anything with a VMFS/ReFS/NTFS/LVM signature above belongs to another system."
  echo "A Kubernetes FC node should only see LUNs that its cluster owns."
}

rescan() {
  for h in /sys/class/fc_host/host*; do
    n=$(basename "$h")
    echo "- - -" > "/sys/class/scsi_host/$n/scan"
    echo "scanned $n"
  done
  sleep 3
  multipath -r >/dev/null 2>&1 || true
}

map_for() {
  local wwid="$1"
  dmsetup ls --target multipath | awk '{print $1}' | while read -r m; do
    [ "$(dmsetup info -c --noheadings -o uuid "$m")" = "mpath-$wwid" ] && echo "$m"
  done
}

show() {
  local wwid="${1:?wwid required}"
  local m; m=$(map_for "$wwid")
  [ -n "$m" ] || { echo "no multipath map for $wwid (try: $0 rescan)"; exit 1; }
  multipath -ll "$m"
  ls -l /dev/disk/by-id/ | grep -i "${wwid}" || true
}

release() {
  local wwid="${1:?wwid required}"
  local m; m=$(map_for "$wwid")
  [ -n "$m" ] || { echo "no multipath map for $wwid, nothing to release"; return 0; }
  local dev=/dev/mapper/$m
  local open; open=$(dmsetup info -c --noheadings -o open "$m" | tr -d ' ')
  local bytes; bytes=$(blockdev --getsize64 "$dev")
  local sig; sig=$(blkid -p -o value -s TYPE "$dev" 2>/dev/null || blkid -p -o value -s PTTYPE "$dev" 2>/dev/null || true)
  local slaves; slaves=$(ls "/sys/block/$(basename "$(readlink -f "$dev")")/slaves")
  local s
  for s in $slaves; do
    [ "$(tr -d ' ' < "/sys/block/$s/device/vendor")" = "COMPELNT" ] || { echo "REFUSE: $s is not COMPELNT"; exit 1; }
  done
  [ "$open" = "0" ] || { echo "REFUSE: $m is open ($open holders)"; exit 1; }
  [ -z "$sig" ] || { echo "REFUSE: $m has a '$sig' signature"; exit 1; }
  [ "$bytes" -le $((MAX_GIB * 1024 * 1024 * 1024)) ] || { echo "REFUSE: $m is larger than ${MAX_GIB}GiB"; exit 1; }
  multipath -f "$m"
  for s in $slaves; do echo 1 > "/sys/block/$s/device/delete"; echo "deleted path $s"; done
  echo "released $wwid; now unmap it on the array"
}

cmd="${1:-inventory}"; shift || true
need_root "$cmd" "$@"
case "$cmd" in
  inventory) inventory ;;
  rescan)    rescan ;;
  show)      show "$@" ;;
  release)   release "$@" ;;
  *) sed -n '2,11p' "$0"; exit 2 ;;
esac
