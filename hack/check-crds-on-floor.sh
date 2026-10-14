#!/usr/bin/env bash
# Creates every CRD in crds/ on a real kube-apiserver at the chart's floor version
# (charts/direwolf-operator/Chart.yaml kubeVersion). The in-process CEL check in
# pkg/api/v1alpha1 uses vendored libraries that back-date newer CEL functions
# (quantity() is 1.29+, listed as 1.28), so only a real old apiserver rejects them (XERK-1678).
#
# Usage: hack/check-crds-on-floor.sh [k8s-version]   (needs go, openssl)
set -euo pipefail

K8S_VERSION="${1:-1.28.3}"
SETUP_ENVTEST="sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.0"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

BIN="$(go run "$SETUP_ENVTEST" use -p path "$K8S_VERSION")"
WORK="$(mktemp -d)"
cleanup() {
  jobs -p | xargs -r kill 2>/dev/null || true
  wait 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT
# Without these, bash keeps running the readiness loop after Ctrl-C has killed the servers.
trap 'exit 130' INT
trap 'exit 143' TERM

free_port() { python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])'; }
ETCD_PORT="$(free_port)"
ETCD_PEER_PORT="$(free_port)"
API_PORT="$(free_port)"

"$BIN/etcd" --data-dir "$WORK/etcd" \
  --listen-client-urls "http://127.0.0.1:$ETCD_PORT" --advertise-client-urls "http://127.0.0.1:$ETCD_PORT" \
  --listen-peer-urls "http://127.0.0.1:$ETCD_PEER_PORT" >"$WORK/etcd.log" 2>&1 &

openssl genrsa -out "$WORK/sa.key" 2048 2>/dev/null
TOKEN="$(openssl rand -hex 16)"
echo "$TOKEN,admin,admin,system:masters" >"$WORK/tokens.csv"

"$BIN/kube-apiserver" --etcd-servers "http://127.0.0.1:$ETCD_PORT" \
  --bind-address 127.0.0.1 --secure-port "$API_PORT" --cert-dir "$WORK/certs" \
  --token-auth-file "$WORK/tokens.csv" --authorization-mode AlwaysAllow \
  --service-account-issuer https://kubernetes.default.svc \
  --service-account-key-file "$WORK/sa.key" --service-account-signing-key-file "$WORK/sa.key" \
  --service-cluster-ip-range 10.0.0.0/24 >"$WORK/apiserver.log" 2>&1 &
API_PID=$!

kubectl() { "$BIN/kubectl" --server "https://127.0.0.1:$API_PORT" --token "$TOKEN" --insecure-skip-tls-verify "$@"; }

for _ in $(seq 60); do
  kubectl get --raw /readyz >/dev/null 2>&1 && break
  kill -0 "$API_PID" 2>/dev/null || break
  sleep 1
done
if ! kubectl get --raw /readyz >/dev/null; then
  tail -n 50 "$WORK/apiserver.log" "$WORK/etcd.log"
  exit 1
fi

echo "kube-apiserver $(kubectl version -o json | python3 -c 'import json,sys; print(json.load(sys.stdin)["serverVersion"]["gitVersion"])')"
# create, not apply: each CRD must be accepted as a fresh install would see it.
kubectl create -f "$ROOT/crds/"
kubectl wait --for condition=Established --timeout 60s -f "$ROOT/crds/"
