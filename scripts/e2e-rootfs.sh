#!/usr/bin/env bash
# e2e for per-group rootfs selection. Drives the installed daemon via ctl.
# Reuses `kalitest` (rootfs=kali, root=yes); spawns+destroys a scratch fedora
# group for isolation + negative cases, and a scratch kali group spawned with
# -rootfs kali -root yes (first-boot check). Run from the guest with the nested
# daemon up.
set -u
export HOME=/var/lib/koto KOTO_HOME=/var/lib/koto KOTO_ADDR=127.0.0.1:8443 \
       KOTO_CREDS_DIR=/var/lib/koto/creds KOTO_CLIENT=tui
K=/usr/local/bin/koto
FED=e2ef
pass=0; fail=0
ok(){ printf '  PASS  %s\n' "$1"; pass=$((pass+1)); }
no(){ printf '  FAIL  %s\n' "$1"; fail=$((fail+1)); }
chk(){ # desc  needle  haystack
  case "$3" in *"$2"*) ok "$1";; *) no "$1 (want '$2' in: $(printf '%s' "$3" | head -c200))";; esac; }
run(){ timeout 120 $K ctl runscript "$1" "$2" 2>&1; }
hdr(){ printf '\n[%s] === %s ===\n' "$(date +%H:%M:%S)" "$1"; }

hdr "1. kali group boots on rootfs-kali.img"
OUT=$(run kalitest 'grep ^ID= /etc/os-release; readlink -f /bin/sh; command -v nmap sqlmap || true')
echo "$OUT"
chk "kali os-release ID=kali"        "ID=kali"        "$OUT"
chk "kali /bin/sh is dash"           "dash"           "$OUT"
chk "kali tool nmap present"         "/usr/bin/nmap"  "$OUT"

hdr "2. root=yes overlay is per-distro (.rootovl-kali) + sudo works"
OUT=$(run kalitest 'ls -d /workspace/.rootovl-kali 2>&1; sudo -n id -u')
echo "$OUT"
chk "kali overlay dir .rootovl-kali"  ".rootovl-kali" "$OUT"
chk "kali passwordless sudo = root"   "0"             "$OUT"

hdr "3. overlay persistence across VM restart"
MARK="/etc/koto-e2e-$(date +%s)"
run kalitest "sudo sh -c 'echo alive > $MARK'; cat $MARK" >/dev/null 2>&1
$K ctl stop kalitest >/dev/null 2>&1; sleep 2
OUT=$(run kalitest "cat $MARK 2>&1")
echo "marker after restart: $OUT"
chk "overlay write survives restart"  "alive"         "$OUT"
run kalitest "sudo rm -f $MARK" >/dev/null 2>&1   # cleanup

hdr "4. default (no rootfs) group boots fedora + overlay .rootovl"
$K ctl spawn "$FED" >/dev/null 2>&1
$K ctl config "$FED" -root yes >/dev/null 2>&1
$K ctl stop "$FED" >/dev/null 2>&1; sleep 2   # posture keys apply on restart
OUT=$(run "$FED" 'grep ^ID= /etc/os-release; test -d /workspace/.rootovl && echo HAVE_ROOTOVL; echo "UID=$(sudo -n id -u 2>/dev/null)"')
echo "$OUT"
chk "default os-release ID=fedora"    "ID=fedora"     "$OUT"
chk "fedora overlay dir .rootovl"     "HAVE_ROOTOVL"  "$OUT"
chk "fedora passwordless sudo = root" "UID=0"         "$OUT"

hdr "5. isolation: fedora group has no kali overlay / no kali marker"
OUT=$(run "$FED" 'test -e /workspace/.rootovl-kali || echo NO_KALI_OVL; ls /etc/koto-e2e-* >/dev/null 2>&1 || echo NO_KALI_MARK')
echo "$OUT"
chk "fedora has NO .rootovl-kali"     "NO_KALI_OVL"   "$OUT"
chk "fedora sees NO kali /etc marker" "NO_KALI_MARK"  "$OUT"

hdr "6. effective config exposes rootfs key"
OUT=$($K ctl config "$FED" 2>&1)
echo "$OUT"
chk "config shows rootfs=fedora"      '"rootfs":"fedora"' "$OUT"
OUT=$($K ctl config kalitest 2>&1)
chk "kali config shows rootfs=kali"   '"rootfs":"kali"' "$OUT"

hdr "7. negative: malformed rootfs name rejected, prior value kept"
OUT=$($K ctl config "$FED" -rootfs 'Bad!Name' 2>&1); echo "$OUT"
chk "malformed name -> still fedora"  '"rootfs":"fedora"' "$OUT"
OUT=$($K ctl config "$FED" -rootfs '../../etc' 2>&1)
chk "path traversal -> still fedora"  '"rootfs":"fedora"' "$OUT"

hdr "8a. negative: well-formed but uninstalled flavor rejected at config"
OUT=$($K ctl config "$FED" -rootfs ubuntu 2>&1); echo "$OUT"
chk "uninstalled flavor -> still fedora" '"rootfs":"fedora"' "$OUT"

hdr "8b. negative: image removed AFTER config -> boot refuses w/ build hint"
IMG=/var/lib/koto/fcassets/rootfs-kali.img
$K ctl stop kalitest >/dev/null 2>&1; sleep 1
mv "$IMG" "$IMG.e2e-bak"; trap '[ -f "$IMG.e2e-bak" ] && mv "$IMG.e2e-bak" "$IMG"' EXIT
OUT=$(timeout 60 $K ctl runscript kalitest 'echo SHOULD_NOT_RUN' 2>&1); echo "$OUT"
mv "$IMG.e2e-bak" "$IMG"
case "$OUT" in *SHOULD_NOT_RUN*) no "boot ran without its rootfs image";; *) ok "boot refused without rootfs image";; esac
chk "error carries a build hint"      "make rootfs"   "$OUT"

hdr "9. spawn -rootfs kali -root yes: right on FIRST boot, no restart"
SK=e2ek
$K ctl destroy "$SK" >/dev/null 2>&1   # leftover from an aborted run
OUT=$($K ctl spawn -rootfs kali -root yes "$SK" 2>&1); RC=$?; echo "$OUT"
[ "$RC" -eq 0 ] && ok "spawn -rootfs kali -root yes accepted" || no "spawn -rootfs kali -root yes rc=$RC"
OUT=$(run "$SK" 'grep ^ID= /etc/os-release; echo "UID=$(sudo -n id -u 2>/dev/null)"')
echo "$OUT"
chk "first boot os-release ID=kali"   "ID=kali"       "$OUT"
chk "first boot sudo uid 0"           "UID=0"         "$OUT"
OUT=$($K ctl config "$SK" 2>&1)
chk "spawned config rootfs=kali"      '"rootfs":"kali"' "$OUT"
chk "spawned config root=yes"         '"root":"yes"'  "$OUT"
$K ctl destroy "$SK" >/dev/null 2>&1

hdr "10. negative: spawn -rootfs nosuch / malformed / bad root error loudly"
SN=e2en
OUT=$($K ctl spawn -rootfs nosuch "$SN" 2>&1); RC=$?; echo "rc=$RC $OUT"
[ "$RC" -ne 0 ] && ok "spawn -rootfs nosuch exits non-zero" || no "spawn -rootfs nosuch succeeded"
chk "error says not installed"        "not installed" "$OUT"
OUT=$($K ctl spawn -rootfs '../etc' "$SN" 2>&1); RC=$?; echo "rc=$RC $OUT"
[ "$RC" -ne 0 ] && ok "spawn -rootfs ../etc exits non-zero" || no "spawn -rootfs ../etc succeeded"
OUT=$($K ctl spawn -root maybe "$SN" 2>&1); RC=$?; echo "rc=$RC $OUT"
[ "$RC" -ne 0 ] && ok "spawn -root maybe exits non-zero" || no "spawn -root maybe succeeded"
OUT=$($K ctl list 2>&1)
case "$OUT" in *"$SN"*) no "rejected spawn left group $SN registered"; $K ctl destroy "$SN" >/dev/null 2>&1;;
               *) ok "rejected spawns registered nothing";; esac

hdr "cleanup: destroy scratch groups, stop kali"
$K ctl destroy "$FED" >/dev/null 2>&1; $K ctl destroy e2ek >/dev/null 2>&1
$K ctl stop kalitest >/dev/null 2>&1

printf '\n==== e2e-rootfs: %d passed, %d failed ====\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
