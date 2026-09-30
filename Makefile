VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X github.com/jgruberf5/roksbnkargoctl/internal/cli.Version=$(VERSION) \
	-X github.com/jgruberf5/roksbnkargoctl/internal/cli.Commit=$(COMMIT) \
	-X github.com/jgruberf5/roksbnkargoctl/internal/cli.BuildDate=$(DATE)
CHECK_IMAGE ?= ghcr.io/jgruberf5/roksbnkargoctl-check
CLI_IMAGE   ?= ghcr.io/jgruberf5/roksbnkargoctl

.PHONY: build check-binary check-image cli-image test vet fmt staticcheck verify far-test book book-pdf
build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/roksbnkargoctl ./cmd/roksbnkargoctl

check-binary:
	CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/check ./cmd/check

check-image:
	docker build -f build/check/Dockerfile --build-arg VERSION=$(VERSION) -t $(CHECK_IMAGE):$(VERSION) .

# roksbnkargoctl itself as an image (what .github/workflows/cli-image.yml publishes).
# EXTRA_CA=corp-ca.pem trusts a TLS-intercepting proxy's CA for the module
# download only (a build secret, never baked into the image).
comma := ,
cli-image:
	docker build -f build/cli/Dockerfile --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) \
		--build-arg DATE=$(DATE) $(if $(EXTRA_CA),--secret id=extra_ca$(comma)src=$(EXTRA_CA)) \
		-t $(CLI_IMAGE):$(VERSION) .

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

# The same checker CI runs (dominikh/staticcheck-action, version latest).
# Without it here, a capitalised error string passed `make verify` and failed CI.
staticcheck:
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

# Everything a change must pass locally.
verify: fmt vet staticcheck test build check-binary

# Renders the real BNK 2.4 GA charts pulled from FAR (never committed).
far-test:
	@test -n "$(ROKSBNKARGOCTL_FAR_TGZ)" || (echo "set ROKSBNKARGOCTL_FAR_TGZ to the FAR auth tarball" && exit 1)
	go test -tags far ./internal/render/ -run Live -v

# The book as HTML (what GitHub Pages publishes).
book:
	mdbook build book

# The book as a PDF plus an HTML archive, into dist/ (needs Docker).
book-pdf:
	scripts/book-pdf.sh $(VERSION)
