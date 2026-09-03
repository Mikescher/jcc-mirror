BINARY := jcc-mirror

DOCKER_REPO=registry.blackforestbytes.com
DOCKER_NAME=mikescher/jcc-mirror
NAMESPACE=$(shell git rev-parse --abbrev-ref HEAD)
HASH=$(shell git rev-parse HEAD)

VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# The build time, not the commit time: the self-updater compares it against the
# Last-Modified of the binary on the share, which is when it was uploaded.
BUILDSTAMP=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS=-X main.version=$(VERSION) -X main.buildStamp=$(BUILDSTAMP)

.PHONY: build test vet run syno syno-arm docker push-docker clean

build:
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

run:
	go run -ldflags "$(LDFLAGS)" . $(ARGS)

# Synology DS with an Intel/AMD CPU
syno:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-amd64 .

# Synology DS with an ARM CPU (DS220j, DS223, ...)
syno-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-arm64 .

docker:
	docker build --build-arg VERSION=$(VERSION) --build-arg BUILDSTAMP=$(BUILDSTAMP) \
	             -t $(DOCKER_NAME):$(HASH) -t $(DOCKER_NAME):$(NAMESPACE)-latest -t $(DOCKER_NAME):latest \
	             -t $(DOCKER_REPO)/$(DOCKER_NAME):$(HASH) -t $(DOCKER_REPO)/$(DOCKER_NAME):$(NAMESPACE)-latest -t $(DOCKER_REPO)/$(DOCKER_NAME):latest \
	             -f deploy/Dockerfile .

push-docker:
	docker image push $(DOCKER_REPO)/$(DOCKER_NAME):$(HASH)
	docker image push $(DOCKER_REPO)/$(DOCKER_NAME):$(NAMESPACE)-latest
	docker image push $(DOCKER_REPO)/$(DOCKER_NAME):latest

clean:
	rm -f $(BINARY) $(BINARY)-amd64 $(BINARY)-arm64
