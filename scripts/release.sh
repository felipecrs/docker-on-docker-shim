#!/usr/bin/env bash

set -euxo pipefail

version="${1}"

echo "Releasing version ${version}..."

script_path=$(realpath "$0")
script_dir=$(dirname "${script_path}")
build_dir="$(realpath "${script_dir}/..")"
readonly build_dir
unset script_path script_dir

docker build "${build_dir}" --pull --push \
  --tag "ghcr.io/felipecrs/dond-shim:${version}" \
  --tag "ghcr.io/felipecrs/dond-shim:latest"
docker build "${build_dir}" --pull --push \
  --target dond-shim-bin --tag "ghcr.io/felipecrs/dond-shim-bin:${version}" \
  --tag "ghcr.io/felipecrs/dond-shim-bin:latest"

git_sha=$(git rev-parse HEAD)
gh release create "v${version}" --title "v${version}" --target "${git_sha}"
