#!/usr/bin/env bash
# End-to-end CSI tests (HPE CSI Driver + scseries-csp).
#   STORAGE_CLASS (default scseries-fc), SNAPSHOT_CLASS (default scseries-snap)
#   ARRAY_CHECK=1 additionally checks the array via hack/sc-maint.py (needs .env with SC_* credentials)
#   hack/csi-e2e.sh basic      RWO filesystem, RWX raw block shared by two pods, online expansion
#   hack/csi-e2e.sh snapshot   VolumeSnapshot -> restore, PVC -> PVC clone (larger), clean teardown
#   hack/csi-e2e.sh all
# Every test deletes its namespace at the end and checks that no PV and no SC volume is left.
set -uo pipefail
cd "$(dirname "$0")/.."
T=deploy/tests
STORAGE_CLASS="${STORAGE_CLASS:-scseries-fc}"
SNAPSHOT_CLASS="${SNAPSHOT_CLASS:-scseries-snap}"
FAIL=0
render() { sed -e "s/STORAGE_CLASS/$STORAGE_CLASS/g" -e "s/SNAPSHOT_CLASS/$SNAPSHOT_CLASS/g" "$@"; }
pvs() { kubectl get pv -o jsonpath="{range .items[?(@.spec.storageClassName=='$STORAGE_CLASS')]}{.metadata.name}{'\n'}{end}" | grep -c . ; }
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAIL=1; }
check() { local want="$1" got="$2" what="$3"; [ "$got" = "$want" ] && pass "$what ($got)" || fail "$what: want '$want', got '$got'"; }

wait_pods() { kubectl -n "$1" wait --for=condition=Ready pod --all --timeout=420s >/dev/null || fail "pods in $1 not ready"; }

teardown() {
  local ns="$1"
  kubectl delete ns "$ns" --wait=true --timeout=420s >/dev/null 2>&1
  for _ in $(seq 1 48); do
    [ "$(pvs)" = "0" ] && break
    sleep 10
  done
  check 0 "$(pvs)" "$ns: no $STORAGE_CLASS PVs left"
  if [ "${ARRAY_CHECK:-0}" = 1 ]; then
    check 0 "$(python3 -I hack/sc-maint.py wipe-volumes --match '^pvc-' | head -1 | awk '{print $1}')" "$ns: no pvc-* volumes left on the array"
  fi
}

basic() {
  echo "== basic"
  render $T/pvc-tests.yaml | kubectl apply -f - >/dev/null
  wait_pods sc-csi-test
  check "Bound Bound" "$(kubectl -n sc-csi-test get pvc fs-rwo block-rwx -o jsonpath='{.items[*].status.phase}')" "PVCs bound"
  kubectl -n sc-csi-test exec block-b -- sh -c 'echo reply-from-block-b | dd of=/dev/xvda bs=512 seek=8 count=1 conv=sync,fsync 2>/dev/null'
  check "hello-from-block-a" "$(kubectl -n sc-csi-test exec block-b -- sh -c 'dd if=/dev/xvda bs=512 count=1 2>/dev/null | head -c 18')" "RWX block: b reads a"
  check "reply-from-block-b" "$(kubectl -n sc-csi-test exec block-a -- sh -c 'dd if=/dev/xvda bs=512 skip=8 count=1 iflag=direct 2>/dev/null | head -c 18')" "RWX block: a reads b"
  kubectl -n sc-csi-test patch pvc fs-rwo -p '{"spec":{"resources":{"requests":{"storage":"3Gi"}}}}' >/dev/null
  for _ in $(seq 1 30); do
    [ "$(kubectl -n sc-csi-test get pvc fs-rwo -o jsonpath='{.status.capacity.storage}')" = "3Gi" ] && break; sleep 5
  done
  check "3Gi" "$(kubectl -n sc-csi-test get pvc fs-rwo -o jsonpath='{.status.capacity.storage}')" "online expansion PVC capacity"
  check "2.9G" "$(kubectl -n sc-csi-test exec fs-writer -- df -h /data | awk 'NR==2{print $2}')" "online expansion filesystem size"
  teardown sc-csi-test
}

snapshot() {
  echo "== snapshot"
  render $T/snapshot-tests.yaml | kubectl apply -f - >/dev/null
  kubectl -n sc-snap-test wait --for=condition=Ready pod/writer --timeout=420s >/dev/null || fail "writer not ready"
  render $T/snapshot-restore.yaml | awk 'BEGIN{RS="---\n"} /kind: VolumeSnapshot\n/' | kubectl apply -f - >/dev/null
  for _ in $(seq 1 60); do
    [ "$(kubectl -n sc-snap-test get volumesnapshot src-snap -o jsonpath='{.status.readyToUse}')" = "true" ] && break; sleep 3
  done
  check "true" "$(kubectl -n sc-snap-test get volumesnapshot src-snap -o jsonpath='{.status.readyToUse}')" "VolumeSnapshot ready"
  kubectl -n sc-snap-test exec writer -- sh -c 'echo CHANGED > /data/marker && sync'
  render $T/snapshot-restore.yaml | kubectl apply -f - >/dev/null
  kubectl -n sc-snap-test wait --for=condition=Ready pod/reader-restored pod/reader-cloned --timeout=420s >/dev/null || fail "readers not ready"
  check "ORIGINAL" "$(kubectl -n sc-snap-test logs reader-restored | head -1)" "restore from snapshot has point-in-time data"
  check "CHANGED" "$(kubectl -n sc-snap-test logs reader-cloned | head -1)" "PVC clone has current data"
  check "2Gi" "$(kubectl -n sc-snap-test get pvc cloned -o jsonpath='{.status.capacity.storage}')" "clone grown to requested size"
  teardown sc-snap-test
}

case "${1:-all}" in
  basic) basic ;;
  snapshot) snapshot ;;
  all) basic; snapshot ;;
  *) sed -n '2,6p' "$0"; exit 2 ;;
esac
[ "$FAIL" = 0 ] && echo "ALL PASSED" || { echo "SOME TESTS FAILED"; exit 1; }
