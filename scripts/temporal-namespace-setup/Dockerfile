# Multi-stage build: compile against the repo's go.mod, ship a static
# binary on distroless. The build context is the repo root
# (compose.yaml: build.context: .), so go.mod / go.sum are one level up
# from this Dockerfile; COPY paths are relative to the context.
FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

# Only the program source for this script; everything else in the repo
# would just bloat the build context and the layer.
COPY scripts/temporal-namespace-setup/main.go ./

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 go build \
	-trimpath \
	-ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
	-o /out/setup-namespace .

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/setup-namespace /usr/local/bin/setup-namespace

# The program does not need a shell, network probes, or filesystem access
# beyond what the runtime gives it. nonroot by default; it only talks to
# gRPC at $TEMPORAL_ADDRESS, which compose resolves to the temporal
# service on the bridge network.
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/setup-namespace"]