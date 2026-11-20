# syntax=docker/dockerfile:1
# Admin assets (CSS/JS/fonts) are prebuilt into internal/admin/static and embedded,
# so the image build needs only Go. Rebuild assets with `make assets` when changing them.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY docs ./docs
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/mino ./cmd/mino \
 && mkdir -p /out/data/uploads

# Static binary on a distroless base: no shell, no package manager, runs as non-root (uid 65532).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mino /mino
COPY --from=build --chown=65532:65532 /out/data /data
ENV MINO_ADDR=:1337 \
    MINO_UPLOAD_DIR=/data/uploads
VOLUME /data
EXPOSE 1337
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/mino", "healthcheck"]
ENTRYPOINT ["/mino"]
