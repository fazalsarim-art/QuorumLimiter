# Multi-stage build: compile a static binary, then ship it on a minimal, nonroot
# Alpine runtime.
FROM golang:1.26.5-alpine3.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/quorumlimiter ./cmd/quorumlimiter

FROM alpine:3.23
RUN addgroup -S -g 10001 app \
 && adduser -S -D -H -u 10001 -G app app \
 && apk add --no-cache ca-certificates tzdata \
 && install -d -o app -g app -m 0700 /data
COPY --from=build /out/quorumlimiter /usr/local/bin/quorumlimiter
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/quorumlimiter"]
