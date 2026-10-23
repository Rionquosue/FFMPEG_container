# syntax=docker/dockerfile:1
ARG GO_IMAGE=golang:1.27.3-alpine3.24@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c
ARG ALPINE_IMAGE=alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
FROM ${GO_IMAGE} AS build
RUN apk add --no-cache build-base linux-headers
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
COPY sandbox ./sandbox
RUN CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/ffmpeg-api ./cmd/server \
 && cc -O2 -static -Wall -Wextra -Werror -fstack-protector-strong \
    -o /out/media-sandbox sandbox/media-sandbox.c

FROM ${ALPINE_IMAGE}
RUN apk add --no-cache ffmpeg \
 && addgroup -g 10001 media \
 && adduser -D -H -u 10001 -G media media \
 && mkdir -p /work /usr/share/ffmpeg-service \
 && chown media:media /work \
 && apk info -vv > /usr/share/ffmpeg-service/packages.txt \
 && ffmpeg -version > /usr/share/ffmpeg-service/ffmpeg-version.txt \
 && ffmpeg -hide_banner -encoders > /usr/share/ffmpeg-service/encoders.txt 2>&1
COPY --from=build /out/ffmpeg-api /out/media-sandbox /usr/local/bin/
USER 10001:10001
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=20s --retries=3 \
 CMD ["/usr/local/bin/ffmpeg-api", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/media-sandbox", "--serve", "/usr/local/bin/ffmpeg-api"]
