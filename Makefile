.PHONY: dev-api dev-web test test-go test-web build release verify clean

GO ?= go
NPM ?= npm
GO_CACHE ?= $(CURDIR)/.cache/go-build

dev-api:
	GOCACHE="$(GO_CACHE)" $(GO) run ./cmd/ialbum

dev-web:
	$(NPM) --prefix web run dev

test: test-go test-web

test-go:
	GOCACHE="$(GO_CACHE)" $(GO) test ./...

test-web:
	$(NPM) --prefix web run test

build:
	$(NPM) --prefix web run build
	mkdir -p bin
	GOCACHE="$(GO_CACHE)" CGO_ENABLED=0 $(GO) build -tags production -trimpath -o bin/ialbum ./cmd/ialbum

release: 
	$(NPM) --prefix web run build
	mkdir -p bin
	GOCACHE="$(GO_CACHE)" CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -tags production -trimpath -o bin/ialbum-linux-amd64 ./cmd/ialbum
	GOCACHE="$(GO_CACHE)" CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -tags production -trimpath -o bin/ialbum-linux-arm64 ./cmd/ialbum
	GOCACHE="$(GO_CACHE)" CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -tags production -trimpath -o bin/ialbum-darwin-amd64 ./cmd/ialbum
	GOCACHE="$(GO_CACHE)" CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -tags production -trimpath -o bin/ialbum-darwin-arm64 ./cmd/ialbum
	GOCACHE="$(GO_CACHE)" CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -tags production -trimpath -o bin/ialbum-windows-amd64.exe ./cmd/ialbum
	GOCACHE="$(GO_CACHE)" CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -tags production -trimpath -o bin/ialbum-windows-arm64.exe ./cmd/ialbum

verify:
	GOCACHE="$(GO_CACHE)" $(GO) test ./...
	GOCACHE="$(GO_CACHE)" $(GO) test -race ./...
	$(NPM) --prefix web run test
	$(NPM) --prefix web run typecheck
	$(NPM) --prefix web run build
	GOCACHE="$(GO_CACHE)" CGO_ENABLED=0 $(GO) build -tags production -trimpath -o bin/ialbum ./cmd/ialbum

clean:
	$(GO) clean -cache
