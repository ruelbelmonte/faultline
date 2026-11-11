# syntax=docker/dockerfile:1

FROM node:22-alpine AS ui
WORKDIR /src/ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci
COPY ui/ ./
# vite.config.ts writes the build to ../internal/ui/dist
RUN npm run build

FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
COPY --from=ui /src/internal/ui/dist/ internal/ui/dist/
ARG VERSION=0.1.0
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X faultline/internal/cli.Version=${VERSION}" -o /out/faultline ./cmd/faultline
# The runtime image has no shell, so the data directory is prepared here.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/faultline /faultline
COPY --from=build --chown=nonroot:nonroot /out/data /data
WORKDIR /data
USER nonroot
EXPOSE 7700 7701 7702
ENTRYPOINT ["/faultline"]
CMD ["version"]
