GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X github.com/d13co/algod-loadb-mesh/internal/agent.Version=$(VERSION)

.PHONY: build test race lint dev clean contract contract-test

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/algod-loadb-mesh ./cmd/algod-loadb-mesh

test:
	$(GO) test ./...

race:
	$(GO) test -race -count=1 ./...

lint:
	$(GO) vet ./...
	test -z "$$(gofmt -l cmd deploy internal test)"

dev: build
	./bin/algod-loadb-mesh dev -nodes 3

# Registry application (AlgoKit project in contract/). Rebuilds the TEAL and
# copies it into the Go package that embeds it.
contract:
	cd contract && npm run build
	cp contract/smart_contracts/artifacts/registry/Registry.approval.teal \
	   contract/smart_contracts/artifacts/registry/Registry.approval.bin \
	   contract/smart_contracts/artifacts/registry/Registry.clear.teal \
	   contract/smart_contracts/artifacts/registry/Registry.clear.bin \
	   contract/smart_contracts/artifacts/registry/Registry.arc56.json \
	   internal/adapters/registryalgo/program/

# Needs `algokit localnet start`.
contract-test:
	cd contract && npm test
	$(GO) test -tags contract -count=1 ./test/contract/

clean:
	rm -rf bin
