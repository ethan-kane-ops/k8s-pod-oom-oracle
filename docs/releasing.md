# Releasing

Maintainer process. Reading a release is covered in
[Security](security.md#supply-chain).

## What a tag publishes

Pushing a `v*` tag runs [`release.yml`](https://github.com/ethan-kane-ops/k8s-pod-oom-oracle/blob/main/.github/workflows/release.yml),
which produces:

| Artifact | Where |
|---|---|
| CLI archives for linux, darwin and windows on amd64 and arm64 | GitHub Release |
| A checksum file, signed with cosign | GitHub Release |
| An SPDX SBOM per archive | GitHub Release |
| A multi-arch container image, signed and attested | `ghcr.io/ethan-kane-ops/k8s-pod-oom-oracle` |

Nothing is built on a laptop. `just release` writes the changelog, tags and
pushes; the workflow does the rest.

!!! note "The daemon is Linux-only"
    Darwin and windows archives exist because `inspect` and `watch` are HTTP
    clients, and the person reading a report is rarely on the node. `daemon`
    needs a Linux kernel and says so if started elsewhere.

## Cutting one

```bash
just release-preview          # what the next version and changelog would be
just release-snapshot         # build every artifact locally, publish nothing
just release minor            # auto | patch | minor | major | vX.Y.Z
```

`just release` refuses to run when the working tree is dirty, when the tag
already exists, when `just check` fails, or when `CHANGELOG.md` still has
content under an `[Unreleased]` heading.

That last check is the awkward one, and it is deliberate. Breaking changes and
migration notes are hand-written above the generated sections, because no commit
subject can say which JSON field a consumer has to rename. git-cliff appends a
section of its own and cannot know those notes belong inside it, so the heading
has to be renamed to the version being cut, by a person, before the tag exists.

## Release notes come from the changelog

`hack/release-notes.sh v0.1.0` prints the `CHANGELOG.md` section for a version.
`just release` runs it before tagging and the workflow runs it before building,
and both fail if the section is empty.

The alternative would be generating notes from commit subjects at release time.
That was the original behaviour, and it published a release whose only mention of
a renamed API field was `fix(oom): filter the victim across PID namespaces`.

## Version metadata

`--version` reports the tag, the full commit and the commit date, injected with
`-ldflags -X` against `internal/version`. The commit date rather than the build
date, so the same source produces the same binary.

## If a release goes wrong

Delete the tag locally and remotely, delete the GitHub Release, and cut the next
patch version. Do not move a tag: the image is signed by digest, and a moved tag
leaves a signature pointing at content nobody can reach.
