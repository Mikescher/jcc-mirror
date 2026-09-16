FROM golang:1.27-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
ARG BUILDSTAMP=unknown
# Static, so the runtime image can be distroless and the same binary can later be
# dropped onto the NAS by the self-updater without a libc to match.
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=$VERSION -X main.buildStamp=$BUILDSTAMP" -o /out/jcc-mirror .

FROM gcr.io/distroless/static-debian12:latest

COPY --from=build /out/jcc-mirror /usr/local/bin/jcc-mirror

# No cap_add, no devices, no network_mode: host. The WireGuard tunnel lives inside
# the process, so no capability beyond the default set is needed.
#
# No USER either: a fixed uid cannot write a bind-mounted NAS share whose ACL does
# not name it, and the image cannot know that uid. Root can, and hands what it
# writes to the owner a pair names. Pin the uid per host with compose's `user:` instead.
WORKDIR /data

# The entrypoint stays the bare binary so `docker compose run ... pairs -data /data`
# reaches the CLI unchanged; the supervisor is the default command instead. It
# runs the daemon as a child and restores the previous binary if a self-update
# will not start - which is why it is a subcommand of ours rather than a shell
# script: this image has no shell (DESIGN.md §5).
ENTRYPOINT ["/usr/local/bin/jcc-mirror"]
CMD ["supervise", "serve"]
