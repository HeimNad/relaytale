FROM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /gateway ./cmd/gateway

FROM build AS test
RUN apk add --no-cache gcc musl-dev
CMD ["go", "test", "-race", "-count=1", "./..."]

FROM alpine:3.21 AS runtime
LABEL org.opencontainers.image.licenses="AGPL-3.0-only"
RUN apk add --no-cache ca-certificates && addgroup -g 10001 gateway && adduser -D -u 10001 -G gateway gateway && mkdir -p /data/eml && chown -R gateway:gateway /data
COPY --from=build /gateway /usr/local/bin/gateway
COPY LICENSE /usr/share/licenses/mailgateway/LICENSE
USER gateway
EXPOSE 8080 587
ENTRYPOINT ["gateway"]
