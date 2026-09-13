FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /relaytale ./cmd/relaytale

FROM build AS test
RUN apk add --no-cache gcc musl-dev make
CMD ["go", "test", "-race", "-count=1", "./..."]

FROM alpine:3.21 AS runtime
LABEL org.opencontainers.image.licenses="AGPL-3.0-only"
RUN apk add --no-cache ca-certificates && addgroup -g 10001 relaytale && adduser -D -u 10001 -G relaytale relaytale && mkdir -p /data/eml && chown -R relaytale:relaytale /data
COPY --from=build /relaytale /usr/local/bin/relaytale
COPY LICENSE /usr/share/licenses/relaytale/LICENSE
USER relaytale
EXPOSE 8080 587
ENTRYPOINT ["relaytale"]
