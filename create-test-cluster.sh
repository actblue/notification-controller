#!/bin/bash
set -u
unset KUBECONFIG
k3d cluster list notifications || k3d cluster create notifications --registry-create k3d-notifications:5001
flux install
kubectl -n flux-system delete deployment notification-controller
#kubectl -n flux-system delete secret notification-controller-dev-pull || kubectl -n flux-system create secret docker-registry notification-controller-dev-pull --docker-username="${GITHUB_USER}" --docker-password="${GITHUB_TOKEN}"

cat << EOF
 Use SKAFFOLD_DEFAULT_REPO=k3d-notifications:5001 skaffold run to deploy

 You must edit your /etc/hosts file and put "k3d-notifications" on the "127.0.0.1" line (it should be on the same
 line -- MacOS has an odd bug when you put in a different entry

 You must also start your docker server with config that includes:

   "insecure-registries" : [ "k3d-notifications:5001" ],
EOF
SKAFFOLD_DEFAULT_REPO=k3d-notifications:5001 skaffold run