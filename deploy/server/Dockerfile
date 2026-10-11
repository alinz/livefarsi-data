###############################################################################
### build stage
###############################################################################
## Stays on the BUILD platform so `go vet` / `go test` run natively instead of
## under QEMU. The binary is cross-compiled for TARGET* below — the tree has no
## cgo, so CGO_ENABLED=0 cross-compiles cleanly.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

## Global ARGs have to be re-declared inside the stage to be visible here.
ARG TARGETOS
ARG TARGETARCH

RUN go vet ./...
RUN go test -count=1 ./...
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-w -s" -o /out/movies ./cmd/server

###############################################################################
### run stage
###############################################################################
## No --platform: this resolves to TARGETPLATFORM, which the build command
## pins to linux/amd64 (see deploy/server/deploy.go).
FROM alpine:3.21

## TLS roots for fetching the source sites over https.
RUN apk add --no-cache ca-certificates

## The sync data lives in /app/data, which docker-compose.yml bind-mounts from
## the host so it survives container restarts and redeploys. The directory is
## created here, owned by the uid the container runs as, so a bare
## `docker run` without a mount still works.
RUN mkdir -p /app/data && chown 1001:1001 /app/data
COPY --from=build /out/movies /app/movies

WORKDIR /app
USER 1001:1001
VOLUME /app/data
EXPOSE 8080

CMD ["/app/movies", "-dir", "/app/data", "-addr", ":8080"]
