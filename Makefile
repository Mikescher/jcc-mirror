BINARY := jcc-mirror

DOCKER_REPO=registry.blackforestbytes.com
DOCKER_NAME=mikescher/jcc-mirror
NAMESPACE=$(shell git rev-parse --abbrev-ref HEAD)
HASH=$(shell git rev-parse HEAD)

.PHONY: build test vet run syno syno-arm docker push-docker clean

build:
	go build -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

run:
	go run . $(ARGS)

# Synology DS with an Intel/AMD CPU
syno:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $(BINARY)-amd64 .

# Synology DS with an ARM CPU (DS220j, DS223, ...)
syno-arm:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o $(BINARY)-arm64 .

docker:
	docker build -t $(DOCKER_NAME):$(HASH) -t $(DOCKER_NAME):$(NAMESPACE)-latest -t $(DOCKER_NAME):latest \
	             -t $(DOCKER_REPO)/$(DOCKER_NAME):$(HASH) -t $(DOCKER_REPO)/$(DOCKER_NAME):$(NAMESPACE)-latest -t $(DOCKER_REPO)/$(DOCKER_NAME):latest \
	             -f deploy/Dockerfile .

push-docker:
	docker image push $(DOCKER_REPO)/$(DOCKER_NAME):$(HASH)
	docker image push $(DOCKER_REPO)/$(DOCKER_NAME):$(NAMESPACE)-latest
	docker image push $(DOCKER_REPO)/$(DOCKER_NAME):latest

clean:
	rm -f $(BINARY) $(BINARY)-amd64 $(BINARY)-arm64
