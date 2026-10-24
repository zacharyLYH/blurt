# Multi-stage build: compile with the Go toolchain, run on a bare alpine.
# modernc.org/sqlite is pure Go, so CGO stays off and the binary runs anywhere.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/blurt .

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S blurt \
    && adduser -S -G blurt blurt \
    && mkdir -p /data \
    && chown -R blurt:blurt /data
COPY --from=build /out/blurt /usr/local/bin/blurt
# SQLite lives on a volume mounted here. A fresh named volume inherits this
# ownership, so the non-root user can write.
VOLUME ["/data"]
USER blurt
EXPOSE 8080
ENTRYPOINT ["blurt"]
