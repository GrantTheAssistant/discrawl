# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.26.5
ARG ALPINE_VERSION=3.23

FROM golang:${GO_VERSION}-alpine AS build
RUN apk add --no-cache ca-certificates git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X github.com/openclaw/discrawl/internal/cli.version=${VERSION}" \
    -o /out/discrawl ./cmd/discrawl
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w" \
    -o /out/discrawl-api ./cmd/discrawl-api

FROM alpine:${ALPINE_VERSION}
RUN apk add --no-cache ca-certificates git openssh-client tzdata \
    && adduser -D -u 10001 -h /home/discrawl discrawl \
    && mkdir -p /data/config /data/data /data/cache /data/state \
    && chown -R discrawl:discrawl /data
ENV HOME=/home/discrawl \
    XDG_CONFIG_HOME=/data/config \
    XDG_DATA_HOME=/data/data \
    XDG_CACHE_HOME=/data/cache \
    XDG_STATE_HOME=/data/state
VOLUME ["/data"]
WORKDIR /data
COPY --from=build /out/discrawl /usr/local/bin/discrawl
COPY --from=build /out/discrawl-api /usr/local/bin/discrawl-api
USER discrawl
ENTRYPOINT ["discrawl"]
CMD ["--help"]
