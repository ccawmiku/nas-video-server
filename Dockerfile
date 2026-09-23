FROM node:24.15.0-bookworm-slim AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web ./
RUN npm run build

FROM golang:1.26.0-bookworm AS backend
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/internal/server/static ./internal/server/static
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/nas-video-server ./cmd/server

FROM debian:13.3-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates tzdata && rm -rf /var/lib/apt/lists/*
RUN useradd --system --uid 10001 --create-home nasvideo
COPY --from=backend /out/nas-video-server /usr/local/bin/nas-video-server
RUN mkdir -p /data /cache && chown -R nasvideo:nasvideo /data /cache
USER nasvideo
ENV DATA_DIR=/data CACHE_DIR=/cache LISTEN_ADDR=:8096 FFMPEG=ffmpeg FFPROBE=ffprobe VAAPI_DEVICE=/dev/dri/renderD128
EXPOSE 8096
ENTRYPOINT ["/usr/local/bin/nas-video-server"]
