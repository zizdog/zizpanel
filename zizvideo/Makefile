GO      ?= go
VERSION ?= 0.1.0-mvp
BIN     ?= dist/govideo
LDFLAGS := -X github.com/zizdog/govideo/internal/api.Version=$(VERSION)

.PHONY: build run test fmt fixtures

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/server

run: build
	./$(BIN)

test:
	$(GO) test ./...

fmt:
	gofmt -l -w .

fixtures:
	./scripts/gen-fixtures.sh
