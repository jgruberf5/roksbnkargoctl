VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/jgruberf5/roksbnkargoctl/internal/cli.Version=$(VERSION) \
	-X github.com/jgruberf5/roksbnkargoctl/internal/cli.Commit=$(COMMIT) \
	-X github.com/jgruberf5/roksbnkargoctl/internal/cli.BuildDate=$(DATE)
CHECK_IMAGE ?= ghcr.io/jgruberf5/roksbnkargoctl-check

.PHONY: build check-binary check-image test vet fmt verify far-test
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/roksbnkargoctl ./cmd/roksbnkargoctl

check-binary:
	CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/check ./cmd/check

check-image:
	docker build -f build/check/Dockerfile --build-arg VERSION=$(VERSION) -t $(CHECK_IMAGE):$(VERSION) .

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

# Everything a change must pass locally.
verify: fmt vet test build check-binary

# Renders the real BNK 2.4 GA charts pulled from FAR (never committed).
far-test:
	@test -n "$(ROKSBNKARGOCTL_FAR_TGZ)" || (echo "set ROKSBNKARGOCTL_FAR_TGZ to the FAR auth tarball" && exit 1)
	go test -tags far ./internal/render/ -run Live -v
