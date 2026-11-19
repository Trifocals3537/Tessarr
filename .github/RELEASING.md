# Tessarr release process

Tessarr releases are deliberate, manually dispatched operations. Do not create
or push `v*` tags directly.

## Cutover prerequisites

Before publishing the first Tessarr release:

1. Merge the release workflow into the protected `main` and `beta` branches.
2. Confirm that required tests and review rules protect both branches.
3. Add an active repository tag ruleset for `v*` that restricts tag creation,
   updates, and deletion.
4. Give only the release workflow credential permission to create release tags.
   Test this boundary in ruleset evaluation mode before enforcing it. Prefer a
   narrowly scoped GitHub App if the repository's built-in workflow identity
   cannot be granted the required bypass safely.
5. Confirm the Tessarr container package is linked to the repository and has
   the intended public visibility.

The tag ruleset is required because historical commits contain an older
tag-triggered release workflow. The current workflow cannot change those
historical files.

## Beta container builds

Open **Actions → Guarded Beta Publishing**, select `beta`, and enter its full
40-character commit SHA and the latest successful **CI** run ID for that exact
revision. Start with the default `validate` mode. It checks approval evidence
without building an image, logging in to the registry, or publishing anything.

Use `build` to test the multi-platform OCI build and digest-preserving local
transfer without publishing. This saves a one-day CI artifact and runs with
read-only repository permissions. Only the separate `publish` job can obtain
package-write permission; it downloads the verified archive by its exact
artifact ID from the same run.

Choose `publish` only when deliberately requesting an image. A push or merge
does not publish. The gate requires the current beta revision, the real CI
workflow, and every required job passing in its current attempt. PR merge
checks, older CI runs, skipped jobs, failed/incomplete runs, forks and other
branches cannot authorize publication. Publishing reruns are refused; retry
with a new explicit dispatch after fixing the cause.

Each image is built from the validated SHA into an OCI archive before registry
login. The gate runs again before upload and before promoting the shared
channel tag, preserving multi-platform manifests, provenance and SBOMs. If beta
or its CI moves during an upload, promotion stops; a version tag may already
exist. Continuous beta versions use `2.7.0-beta.RUN_ID.SHA`, where `RUN_ID` is
globally unique and `SHA` is the seven-character source prefix. The existing
version badge continues linking to that source commit.

The old **Beta Docker Build** workflow (`beta-docker.yml`) is retired and must
remain disabled in GitHub. Keeping its workflow identity disabled also prevents
delayed events from historical push-triggered definitions from publishing.
The new workflow exists on both protected `main` and `beta` branches.

## Versioned releases

Open **Actions**, choose **Release**, and select the source branch that matches
the requested version:

- For `vMAJOR.MINOR.PATCH-beta.N` or `vMAJOR.MINOR.PATCH-rc.N`, select `beta`.
- For `vMAJOR.MINOR.PATCH`, select `main`.

Enter the complete tag in the workflow input. The workflow rejects malformed
or existing tags and verifies the selected branch. Every job builds the
immutable commit associated with that dispatch. Runs from the same branch are
serialized so shared channel tags cannot race each other.

Beta/RC releases also require `expected_sha` and `ci_run_id` and use the same
gate and final publication checks as continuous beta images. Both paths share
the `beta-publishing` concurrency group. Stable releases retain their existing
manual process and do not require these beta-only inputs.

The workflow builds and validates all platform artifacts, publishes the
container image, then creates the GitHub release and its tag. A failed run does
not silently reuse an existing release tag.

## Stable promotion

Promote the already validated release candidate from `beta` to `main` through
the protected pull-request process. Dispatch the stable release from `main`
only after the exact promoted commit and all required checks are confirmed.

Never move or replace a published release tag. Correct a bad release with a new
version and preserve the previous artifacts for rollback.
