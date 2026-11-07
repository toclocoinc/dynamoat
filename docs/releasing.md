# Cutting a release

A release is triggered by pushing a tag named `vX.Y.Z` to the repository. The
`.github/workflows/release.yml` workflow does the rest; nothing is done by hand.

## Before you tag

1. Update `CHANGELOG.md` with a `## vX.Y.Z` section describing the changes for
   this release. The workflow uses that section as the GitHub release body; if
   there is no matching section it falls back to a link to the changelog.
2. Set `version` and `appVersion` in `deploy/helm/dynamoat/Chart.yaml` to
   `X.Y.Z` (no `v`). The chart pulls the image tagged with its `appVersion`.
3. Make sure `scripts/test.sh` passes locally:

   ```
   scripts/test.sh
   ```

4. Commit the changelog (and any other changes), then tag and push:

   ```
   git tag vX.Y.Z
   git push origin vX.Y.Z
   ```

Tags containing a hyphen (`v0.1.0-rc.1`) are marked as prereleases.

## What the workflow produces

Pushing the tag runs `scripts/test.sh` first; the release job does not start
unless it passes. The release job then:

- Builds static binaries (CGO disabled, trimmed, stripped) for
  `linux/amd64`, `linux/arm64`, `darwin/amd64` and `darwin/arm64`, each with
  the version and commit baked in via linker flags, and packages each as
  `dynamoat_<version>_<os>_<arch>.tar.gz` together with `LICENSE`, `NOTICE` and
  `README.md` (whichever are present).
- Writes `SHA256SUMS` covering the four archives.
- Creates the GitHub release for the tag, attaching the archives and
  `SHA256SUMS`, with the body taken from the tag's section of `CHANGELOG.md`.
- Builds and pushes a multi-arch (`linux/amd64`, `linux/arm64`) image to
  `ghcr.io/<owner>/dynamoat`, tagged with the version both with and without
  the leading `v` (`v0.1.1` and `0.1.1`), and `latest` for non-prerelease
  tags.

## Verifying a release

Download the archive for your platform from the release page, then check the
checksums from the repo (or the release page) before extracting:

```
shasum -a 256 dynamoat_<version>_<os>_<arch>.tar.gz
cat SHA256SUMS
tar -xzf dynamoat_<version>_<os>_<arch>.tar.gz
./dynamoat_<version>_<os>_<arch>/dynamoat --version
```

The image reports the same version and commit:

```
docker run --rm ghcr.io/<owner>/dynamoat:<version> --version
```

Replace `<owner>` with the repository owner (for example `toclocoinc`) and
`<version>` with the tag such as `v0.1.0`.
