#!/usr/bin/env bash

set -euxo pipefail

version="${1}"

echo "Releasing version ${version}..."

script_path=$(realpath "$0")
script_dir=$(dirname "${script_path}")
build_dir="$(realpath "${script_dir}/..")"
readonly build_dir
unset script_path script_dir

readonly platforms="linux/amd64,linux/arm64"

docker buildx build "${build_dir}" --pull --push --platform "${platforms}" \
  --build-arg "VERSION=${version}" \
  --tag "ghcr.io/felipecrs/dond-shim:${version}" \
  --tag "ghcr.io/felipecrs/dond-shim:latest"
docker buildx build "${build_dir}" --pull --push --platform "${platforms}" \
  --build-arg "VERSION=${version}" \
  --target dond-shim-bin --tag "ghcr.io/felipecrs/dond-shim-bin:${version}" \
  --tag "ghcr.io/felipecrs/dond-shim-bin:latest"

git_sha=$(git rev-parse HEAD)
gh release create "v${version}" --title "v${version}" --target "${git_sha}"
