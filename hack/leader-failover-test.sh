#!/usr/bin/env bash
# Leader failover during provisioning: create N PVCs with pods, delete the CSP leader pod a few seconds in.
# Expect: all pods Ready, exactly N SC volumes (idempotent creates, no duplicates), a new leader, clean teardown.
#   hack/leader-failover-test.sh [N]
set -uo pipefail
cd "$(dirname "$0")/.."
N="${1:-6}"
NS=sc-leader-test
STORAGE_CLASS="${STORAGE_CLASS:-scseries-fc}"
CSP_NS="${CSP_NS:-hpe-storage}"
LEASE="${LEASE:-scseries-csp}"   # = Helm release fullname
FAIL=0
check() { [ "$2" = "$1" ] && echo "PASS  $3 ($2)" || { echo "FAIL  $3: want '$1', got '$2'"; FAIL=1; }; }

{
  echo "apiVersion: v1"; echo "kind: Namespace"; echo "metadata: {name: $NS}"
  for i in $(seq 1 "$N"); do
    cat <<EOF
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: v$i, namespace: $NS}
spec:
  storageClassName: $STORAGE_CLASS
  volumeMode: Block
  accessModes: [ReadWriteOnce]
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: p$i, namespace: $NS}
spec:
  containers:
    - {name: c, image: docker.io/library/busybox:1.37.0, command: ["sleep", "1d"], volumeDevices: [{name: v, devicePath: /dev/xvda}]}
  volumes:
    - {name: v, persistentVolumeClaim: {claimName: v$i}}
EOF
  done
} > /tmp/leader-failover-$$.yaml

before=$(kubectl -n "$CSP_NS" get lease "$LEASE" -o jsonpath='{.spec.holderIdentity}')
kubectl apply -f /tmp/leader-failover-$$.yaml >/dev/null
sleep 4
leaderpod=${before%%@*}
echo "deleting leader $leaderpod during provisioning"
kubectl -n "$CSP_NS" delete pod "$leaderpod" --wait=false >/dev/null
kubectl -n $NS wait --for=condition=Ready pod --all --timeout=600s >/dev/null || FAIL=1
after=$(kubectl -n "$CSP_NS" get lease "$LEASE" -o jsonpath='{.spec.holderIdentity}')
[ "${after%%@*}" != "$leaderpod" ] && echo "PASS  new leader ${after%%@*}" || { echo "FAIL  leader unchanged"; FAIL=1; }
check "$N" "$(kubectl -n $NS get pods --no-headers | grep -c ' Running ')" "pods running"
if [ "${ARRAY_CHECK:-0}" = 1 ]; then
  check "$N" "$(python3 -I hack/sc-maint.py wipe-volumes --match '^pvc-' | head -1 | awk '{print $1}')" "exactly N volumes on the array"
fi
kubectl delete ns $NS --wait=true --timeout=600s >/dev/null
until [ "$(kubectl get pv -o jsonpath="{range .items[?(@.spec.storageClassName=='$STORAGE_CLASS')]}{.metadata.name}{'\n'}{end}" | grep -c .)" = "0" ]; do sleep 10; done
if [ "${ARRAY_CHECK:-0}" = 1 ]; then
  check 0 "$(python3 -I hack/sc-maint.py wipe-volumes --match '^pvc-' | head -1 | awk '{print $1}')" "teardown: no volumes left"
fi
rm -f /tmp/leader-failover-$$.yaml
[ "$FAIL" = 0 ] && echo "ALL PASSED" || { echo "SOME TESTS FAILED"; exit 1; }
