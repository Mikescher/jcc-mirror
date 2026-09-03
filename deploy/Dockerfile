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

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/jcc-mirror /usr/local/bin/jcc-mirror

# No cap_add, no devices, no network_mode: host. The WireGuard tunnel lives inside
# the process, so an ordinary unprivileged container is all that is needed.
USER nonroot:nonroot
WORKDIR /data

ENTRYPOINT ["/usr/local/bin/jcc-mirror"]
CMD ["serve"]
