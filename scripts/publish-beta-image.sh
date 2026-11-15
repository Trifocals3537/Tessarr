#!/usr/bin/env bash
set -euo pipefail

archive="${1:?OCI archive required}"
version="${2:?Beta version required}"
channel="${3:-beta}"
image=ghcr.io/trifocals3537/tessarr

if [[ ! -f "$archive" || -L "$archive" || ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+-(beta|rc)(\.[0-9a-z]+)*$ || ! "$channel" =~ ^(beta|rc)$ ]]; then
    echo 'Invalid beta archive, version, or channel' >&2
    exit 1
fi
if [[ "$version" != *"-$channel" && "$version" != *"-$channel."* ]]; then
    echo 'Version does not match its channel' >&2
    exit 1
fi
if [[ "${BETA_MODE:-}" != publish ]]; then
    echo 'An explicit publish request is required' >&2
    exit 1
fi

# Build completed without registry credentials. Recheck before any upload and
# again before advancing the shared channel tag; preserve OCI attestations.
node scripts/beta-publish-gate.mjs
printf '%s' "${GH_TOKEN:?GitHub publishing token required}" |
    skopeo login --username "${GITHUB_ACTOR:?GitHub actor required}" --password-stdin ghcr.io
trap 'skopeo logout ghcr.io >/dev/null 2>&1 || true' EXIT
skopeo copy --all --preserve-digests "oci-archive:$archive" "docker://$image:$version"
node scripts/beta-publish-gate.mjs
skopeo copy --all --preserve-digests "docker://$image:$version" "docker://$image:$channel"
printf 'Published %s:%s and %s:%s\n' "$image" "$version" "$image" "$channel"
