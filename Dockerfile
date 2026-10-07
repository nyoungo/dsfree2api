# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOOS=linux GOPROXY=https://goproxy.cn,https://proxy.golang.org,direct
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -trimpath -ldflags "-s -w" -o /out/dsfree2api ./cmd/dsfree2api

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 app
WORKDIR /app
COPY --from=build /out/dsfree2api /usr/local/bin/dsfree2api
COPY config.example.toml /app/config.toml
USER app
EXPOSE 8000 8001
ENV HOST=0.0.0.0 PORT=8000 ADMIN_ENABLED=true DATA_DIR=/app/data
VOLUME ["/app/data"]
ENTRYPOINT ["dsfree2api"]
CMD ["-config", "/app/config.toml"]
