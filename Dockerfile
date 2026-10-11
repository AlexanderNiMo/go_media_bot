# Build stage.
FROM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first: this layer is cached until go.mod or go.sum changes, so
# editing source does not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown

# CGO is off and the target is static, so the result runs on a scratch image.
# There is no cgo dependency to lose: the Postgres driver is pure Go.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
    -o /out/mediad ./cmd/mediad

# Runtime stage.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/mediad /usr/local/bin/mediad

# Runs as nonroot; the binary needs no filesystem access of its own. The
# database lives in Postgres and the workflow state in Temporal.
USER nonroot:nonroot

EXPOSE 8080

# Overridden by the compose file, which picks worker, api or all.
ENTRYPOINT ["/usr/local/bin/mediad"]
CMD ["all"]
