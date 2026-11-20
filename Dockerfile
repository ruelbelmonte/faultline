# syntax=docker/dockerfile:1

# Base images are pinned by digest (the tag is kept for readers and for
# Dependabot, which updates both). The build stages run on the build host's
# platform and cross-compile, so multi-platform images need no emulation.

FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:6bd9ab43c0ec0190ca7b2d193906cb8b7edf6e7f4afa43ddeccd6452710bffb5 AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
# vite.config.ts writes the build to ../internal/ui/dist
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.27@sha256:ec47ea3725fea0cc88f6de94919aef19f40f12fc1ec0e2d1b3426dbf15ecca04 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=ui /src/internal/ui/dist/ internal/ui/dist/
ARG VERSION=0.1.0
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-s -w -X faultline/internal/cli.Version=${VERSION}" -o /out/faultline ./cmd/faultline
# The runtime image has no shell, so the data directory is prepared here.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/faultline /faultline
COPY --from=build --chown=nonroot:nonroot /out/data /data
WORKDIR /data
USER nonroot
EXPOSE 7700 7701 7702
ENTRYPOINT ["/faultline"]
CMD ["version"]
