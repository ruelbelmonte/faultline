# Releasing

Releases are built by `.github/workflows/release.yaml`. Pushing a tag `vMAJOR.MINOR.PATCH` (optionally `-PRERELEASE`) on `master` publishes one. Nothing else does.

## What a release contains

| Artifact | Where | How to check it |
|---|---|---|
| `faultline_<version>_<os>_<arch>[.exe]` and `faultline-demo_<version>_<os>_<arch>[.exe]` for linux, darwin and windows on amd64 and arm64 | GitHub release assets | `sha256sum --ignore-missing -c checksums.txt`; `gh attestation verify <file> --repo ruelbelmonte/faultline` |
| `checksums.txt` (SHA-256 of every asset) | GitHub release assets | |
| `faultline_<version>.spdx.json`, an SPDX SBOM of the source tree (Syft) | GitHub release assets | |
| `ghcr.io/ruelbelmonte/faultline:<version>` and `ghcr.io/ruelbelmonte/faultline-demo:<version>`, linux/amd64 and linux/arm64, also tagged `latest` unless the version is a prerelease | GHCR | see below |

Each image carries a BuildKit SBOM and provenance attestation (`provenance: mode=max`, `sbom: true`), a keyless cosign signature, and a GitHub build provenance attestation:

```sh
cosign verify ghcr.io/ruelbelmonte/faultline:0.1.0 \
  --certificate-identity-regexp '^https://github.com/ruelbelmonte/faultline/\.github/workflows/release\.yaml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/ruelbelmonte/faultline:0.1.0 --repo ruelbelmonte/faultline
```

The version comes from the tag (`v0.1.0` gives `0.1.0`) and is stamped into the binaries (`faultline version`) and the images (`--build-arg VERSION`). Base images are pinned by digest in both Dockerfiles; Dependabot updates the digests.

## Dry runs

`workflow_dispatch` (Actions, release, Run workflow, with a version such as `0.0.0-dryrun`) and every pull request that changes the workflow or a Dockerfile run the same build without logging in, pushing, signing, attesting or releasing. The binaries, checksums and SBOM are kept as the `dist` workflow artifact. Manual dispatch is only available once the workflow is on `master`.

## Cutting v0.1.0

1. Make sure `master` is green in CI and the last pull request run of the release workflow (a dry run) passed.
2. Optional: run the dry run by hand on `master` (Actions, release, Run workflow) and download the `dist` artifact to try a binary.
3. Check one repository setting: Settings, Actions, General, Workflow permissions can stay at the read-only default, because the workflow asks for the extra permissions each job needs (`contents: write` to create the release, `packages: write` to push images, `id-token: write` and `attestations: write` to sign and attest).
4. Tag and push from an up-to-date `master`:
   ```sh
   git checkout master && git pull
   git tag -a v0.1.0 -m "Faultline 0.1.0"
   git push origin v0.1.0
   ```
5. Watch the release workflow. It refuses a tag that is not on `master`'s history.
6. After it finishes:
   - The GHCR packages are created private. Make them public if the images should be pullable without logging in: your profile, Packages, `faultline` and `faultline-demo`, Package settings, Change visibility. Link each package to the repository there too.
   - Check the release notes GitHub generated, and edit them to state the breaking change in this version: experiments with `traffic.command` need `control.allow_commands: true` (D101).
   - Run the verification commands above against the published artifacts.
7. If something is wrong, delete the GitHub release and the tag (`git push --delete origin v0.1.0`), delete the image versions on GHCR, fix, and tag again. Never move a tag that has been published and pulled; release `v0.1.1` instead.
