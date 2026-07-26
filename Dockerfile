# --- UI build ---
FROM node:22-alpine AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json web/.npmrc ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- Go build ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/web/dist ./web/dist
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -tags embedui \
    -ldflags "-X babki.my/babki/internal/platform/version.Version=${VERSION}" \
    -o /out/babki ./cmd/babki

# --- Runtime ---
FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S babki && adduser -S babki -G babki
USER babki
COPY --from=build /out/babki /usr/local/bin/babki
EXPOSE 8080
ENTRYPOINT ["babki"]
CMD ["all"]
