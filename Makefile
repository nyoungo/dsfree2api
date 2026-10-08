# dsfree2api — build / test helpers
GO      ?= go
VERSION ?= 0.6.4
LDFLAGS := -s -w -X github.com/nyoungo/dsfree2api/internal/admin.Version=$(VERSION)
BIN     := bin/dsfree2api
IMAGE   ?= dsfree2api:latest

.PHONY: build run test vet fmt tidy check linux docker clean

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/dsfree2api

run: build
	./$(BIN)

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -w .

tidy:
	$(GO) go mod tidy

check: fmt vet test

# Static linux/amd64 binary for servers (scp it over and run it).
linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath \
		-ldflags "$(LDFLAGS)" -o bin/dsfree2api-linux-amd64 ./cmd/dsfree2api

linux-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath \
		-ldflags "$(LDFLAGS)" -o bin/dsfree2api-linux-arm64 ./cmd/dsfree2api

docker:
	docker build -t $(IMAGE) .

clean:
	rm -rf bin
