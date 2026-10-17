# syntax=docker/dockerfile:1

# The schmeide control node image. It runs `schmeide control` by default.
# Runner mode needs git and a harness CLI, which this image does not have, so
# runners run from a binary on the runner machine instead. See
# docs/DEPLOYMENT.md.
#
#   docker build --build-arg VERSION=v0.0.1 -t schmeide .

# The build runs on the builder's own platform and cross-compiles, so a
# multi-arch build needs no emulation. Keep the Go version in step with
# go.mod and the Makefile's GOTOOLCHAIN.
FROM --platform=$BUILDPLATFORM golang:1.26.9 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-X github.com/autobutler-org/schmeide/internal/release.Version=${VERSION}" \
    -o /out/schmeide ./cmd/schmeide \
 && mkdir /out/data

# distroless static has CA certificates for the GitHub API, and no shell.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/schmeide /usr/local/bin/schmeide
# Owned by nonroot, so a fresh named volume at /data is writable.
COPY --from=build --chown=65532:65532 /out/data /data
ENV SCHMEIDE_DATA_DIR=/data
VOLUME /data
USER 65532:65532
EXPOSE 8080 4222
ENTRYPOINT ["schmeide"]
CMD ["control"]
