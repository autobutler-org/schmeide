# Versioning

## The version stamp

The schmeide binary carries one version string, `release.Version` in
`internal/release`. `make build` sets it with the linker:

```sh
go build -ldflags "-X github.com/autobutler-org/schmeide/internal/release.Version=$(VERSION)" ./cmd/schmeide
```

`VERSION` defaults to `git describe --tags --always --dirty`, so a build at a
tag such as `v1.2.0` reports `v1.2.0`. Override it with `make build VERSION=v1.2.0`.
A build without the stamp, such as `go run ./cmd/schmeide` or `go install`,
reports `dev`.

Print it with:

```sh
schmeide version
```

## Runner and control node skew

A runner sends its version in the registration it publishes when it starts.
When the control node sees a registration, it compares that version with its
own. If both are semantic versions (`v1.2.0`, not `dev` or a bare commit
hash) and they differ, the control node logs a warning naming the steward,
host, and both versions. It still accepts the runner. Upgrade whichever side
is behind so the two match.

## Cutting a changelog

```sh
gh auth login   # once
make changelog
```

`make changelog` writes `CHANGELOG.md` from the repository's merged pull
requests, newest first, one line per pull request with its number and merge
date. It needs the GitHub CLI, `gh`, installed and logged in; without it the
target exits non-zero and says so. Review the file and commit it yourself.

## Publishing an image

Pushing a `v*` tag runs `.github/workflows/release.yml`. It builds the
`Dockerfile` for linux/amd64 and linux/arm64, stamped with the tag as
`VERSION` the way `make build` stamps the binary, and pushes
`ghcr.io/autobutler-org/schmeide:<version>` with the `v` dropped, plus
`latest`. Tag `v0.0.1` publishes `ghcr.io/autobutler-org/schmeide:0.0.1`:

```sh
git tag v0.0.1
git push origin v0.0.1
```

The image runs the control node only. Runner mode needs git and a harness
CLI, which the image does not have. Every pull request builds the image
without pushing it, so the Dockerfile cannot rot. How to run the image is in
[DEPLOYMENT.md](DEPLOYMENT.md).

Build one locally with:

```sh
docker build --build-arg VERSION=$(git describe --tags --always --dirty) -t schmeide .
```

## Out of scope

Publishing binaries is out of scope. Nothing here uploads binaries or creates
a GitHub release. Tag a commit and build from it with `make build` when you
need a stamped binary.
