# The UI and the binary are built on the build machine's own architecture and
# only the last stage is the target's: Go cross-compiles, and the page is the
# same on every architecture. An arm64 image then needs no emulated npm or Go
# build, only an emulated `apk add`.

# --- UI build ---
FROM --platform=$BUILDPLATFORM node:22-alpine AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- Go build ---
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/web/dist ./web/dist
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -tags embedui \
    -ldflags "-X babki.my/babki/internal/platform/version.Version=${VERSION}" \
    -o /out/babki ./cmd/babki

# --- Runtime ---
FROM alpine:3.24
LABEL org.opencontainers.image.source="https://github.com/alshstf/babki.my" \
      org.opencontainers.image.description="Учёт личных и семейных финансов с фокусом на инвестиции" \
      org.opencontainers.image.licenses="FSL-1.1-ALv2"
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S babki && adduser -S babki -G babki
USER babki
COPY --from=build /out/babki /usr/local/bin/babki
EXPOSE 8080
ENTRYPOINT ["babki"]
CMD ["all"]
