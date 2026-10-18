#!/usr/bin/env bash
# Build the CSP image without a container engine (go build + crane), equivalent to the Dockerfile.
#   hack/build-image.sh [VERSION]            -> dist/scseries-csp-VERSION.tar
#   IMPORT=1 hack/build-image.sh [VERSION]   ... and import into the local RKE2/k3s containerd (k8s.io namespace)
#   PUSH=1 hack/build-image.sh [VERSION]     ... and push to $IMAGE (needs registry credentials for crane)
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${1:-0.2.0}"
IMAGE="${IMAGE_REPO:-ghcr.io/kube-dc/scseries-csp}:${VERSION}"
BASE="gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab"
CRANE="${CRANE:-crane}"
GO="${GO:-go}"
command -v "$CRANE" >/dev/null || { echo "crane not found (set CRANE=/path/to/crane)"; exit 1; }

work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
mkdir -p "$work/root"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$GO" build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
  -o "$work/root/scseries-csp" ./cmd/scseries-csp
tar --owner=0 --group=0 --numeric-owner --mtime='2026-01-01' -C "$work/root" -cf "$work/layer.tar" scseries-csp

mkdir -p dist
out="dist/scseries-csp-${VERSION}.tar"
"$CRANE" mutate "$BASE" --platform linux/amd64 \
  --append "$work/layer.tar" \
  --entrypoint /scseries-csp --cmd serve \
  --user 65532:65532 \
  --exposed-ports 8080,9090 \
  -l org.opencontainers.image.title=scseries-csp \
  -l org.opencontainers.image.version="$VERSION" \
  -l org.opencontainers.image.source=https://github.com/kube-dc/scseries-csp \
  -l org.opencontainers.image.base.name="$BASE" \
  -t "$IMAGE" -o "$out"
echo "built $IMAGE -> $out ($(du -h "$out" | cut -f1))"

if [ "${IMPORT:-0}" = 1 ]; then
  CTR="${CTR:-/var/lib/rancher/rke2/bin/ctr}"
  SOCK="${CONTAINERD_SOCK:-/run/k3s/containerd/containerd.sock}"
  sudo "$CTR" -a "$SOCK" -n k8s.io images import "$out" >/dev/null
  sudo "$CTR" -a "$SOCK" -n k8s.io images ls -q | grep -F "$IMAGE"
fi
if [ "${PUSH:-0}" = 1 ]; then
  "$CRANE" push "$out" "$IMAGE"
fi
