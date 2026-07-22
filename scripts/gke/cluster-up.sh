#!/usr/bin/env bash
set -euo pipefail

project="${SANDBOX_GKE_PROJECT:-glab-384109}"
zone="${SANDBOX_GKE_ZONE:-asia-east1-b}"
cluster="${SANDBOX_GKE_CLUSTER:-agent-sandbox-e2e}"
context="gke_${project}_${zone}_${cluster}"
[[ "${project}/${zone}/${cluster}" == "glab-384109/asia-east1-b/agent-sandbox-e2e" ]] || {
  echo "ERROR: refusing non-E2E target ${project}/${zone}/${cluster}" >&2
  exit 1
}

if ! gcloud container clusters describe "${cluster}" --project "${project}" --zone "${zone}" >/dev/null 2>&1; then
  gcloud container clusters create "${cluster}" \
    --project "${project}" \
    --zone "${zone}" \
    --release-channel regular \
    --num-nodes 1 \
    --machine-type e2-standard-2 \
    --disk-type pd-balanced \
    --disk-size 30 \
    --image-type COS_CONTAINERD \
    --enable-ip-alias \
    --enable-dataplane-v2 \
    --enable-shielded-nodes \
    --workload-pool "${project}.svc.id.goog" \
    --metadata disable-legacy-endpoints=true \
    --scopes=https://www.googleapis.com/auth/devstorage.read_only,https://www.googleapis.com/auth/logging.write,https://www.googleapis.com/auth/monitoring \
    --quiet
fi

gcloud container clusters get-credentials "${cluster}" --project "${project}" --zone "${zone}" >/dev/null
if ! gcloud container node-pools describe sandbox-gvisor --project "${project}" --cluster "${cluster}" --zone "${zone}" >/dev/null 2>&1; then
  gcloud container node-pools create sandbox-gvisor \
    --project "${project}" \
    --cluster "${cluster}" \
    --zone "${zone}" \
    --num-nodes 2 \
    --machine-type e2-standard-2 \
    --disk-type pd-balanced \
    --disk-size 30 \
    --image-type COS_CONTAINERD \
    --sandbox type=gvisor \
    --metadata disable-legacy-endpoints=true \
    --scopes=https://www.googleapis.com/auth/devstorage.read_only,https://www.googleapis.com/auth/logging.write,https://www.googleapis.com/auth/monitoring \
    --quiet
fi

nodes="$(kubectl --context "${context}" get nodes -o json | jq '.items | length')"
[[ "${nodes}" == 3 ]] || { echo "ERROR: expected exactly 3 nodes, found ${nodes}" >&2; exit 1; }
kubectl --context "${context}" get runtimeclass gvisor >/dev/null
kubectl --context "${context}" get nodes -L cloud.google.com/gke-nodepool,sandbox.gke.io/runtime
printf 'GKE E2E cluster is ready: %s\n' "${context}"
