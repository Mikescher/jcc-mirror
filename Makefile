BINARY := jcc-mirror

DOCKER_NAME=mikescher/jcc-mirror
NAMESPACE=$(shell git rev-parse --abbrev-ref HEAD)
HASH=$(shell git rev-parse HEAD)

VERSION=$(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
# The build time, not the commit time: the self-updater compares it against the
# Last-Modified of the binary on the share, which is when it was uploaded.
BUILDSTAMP:=$(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS=-X main.version=$(VERSION) -X main.buildStamp=$(BUILDSTAMP)

RELEASE_DAV=https://cloud.mikescher.com/public.php/dav/files/2JwdMYXzG7gpFBc

.PHONY: build web test vet run syno syno-arm docker push-docker release release-build clean

WEB_DIST := web/dist/index.html
WEB_SRC := $(shell find web/src web/public -type f) web/angular.json web/tsconfig.json web/tsconfig.app.json
# npm writes this file on every install, so it dates the last `npm ci`.
WEB_DEPS := web/node_modules/.package-lock.json

build: $(WEB_DIST)
	go build -ldflags "$(LDFLAGS)" -o $(BINARY) .

# web/dist is not checked in, but the web package embeds it, so no Go package
# compiles until it has been built. Every Go target depends on it; it is only
# rebuilt when something under web/ is newer.
web: $(WEB_DIST)

$(WEB_DIST): $(WEB_SRC) $(WEB_DEPS)
	cd web && npx ng build

$(WEB_DEPS): web/package.json web/package-lock.json
	cd web && npm ci

test: $(WEB_DIST)
	go test ./...

vet: $(WEB_DIST)
	go vet ./...

run: $(WEB_DIST)
	go run -ldflags "$(LDFLAGS)" . $(ARGS)

# Synology DS with an Intel/AMD CPU
syno: $(WEB_DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-amd64 .

# Synology DS with an ARM CPU (DS220j, DS223, ...)
syno-arm: $(WEB_DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o $(BINARY)-arm64 .

docker: $(WEB_DIST)
	docker build --build-arg VERSION=$(VERSION) --build-arg BUILDSTAMP=$(BUILDSTAMP) \
	             -t $(DOCKER_NAME):$(HASH) -t $(DOCKER_NAME):$(NAMESPACE)-latest -t $(DOCKER_NAME):latest \
	             -f deploy/Dockerfile .

push-docker:
	docker image push $(DOCKER_NAME):$(HASH)
	docker image push $(DOCKER_NAME):$(NAMESPACE)-latest
	docker image push $(DOCKER_NAME):latest

# The binary the self-updater fetches runs inside the container, so it is built
# like the one in the image: static, linux/amd64.
# Commits pending changes first, so the version is not "-dirty". The build runs in
# a sub-make because VERSION is expanded before a recipe's first line runs.
release:
	@if [ -n "$$(git status --porcelain --untracked-files=no)" ]; then \
	    git status --short --untracked-files=no; \
	    printf 'Commit message: '; read -r msg; \
	    [ -n "$$msg" ] || { echo 'aborted: empty commit message' >&2; exit 1; }; \
	    git commit -a -m "$$msg"; \
	fi
	$(MAKE) release-build

release-build: docker
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o $(BINARY) .
	$(MAKE) push-docker
	curl --fail --show-error -X PUT \
	     -H 'Content-Type: application/octet-stream' \
	     --upload-file $(BINARY) \
	     '$(RELEASE_DAV)/$(BINARY)'

clean:
	rm -f $(BINARY) $(BINARY)-amd64 $(BINARY)-arm64
