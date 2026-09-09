ARG GO_IMAGE=golang:1.26-bookworm@sha256:9fdc884aacc3bec89b20ffc69f4bb369c78210e3e4f600387b5128b12c199f81
ARG RUNTIME_IMAGE=gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

FROM ${GO_IMAGE} AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY gen ./gen
COPY cmd ./cmd
COPY internal ./internal

ARG SERVICE
ARG VERSION=dev
RUN test -n "${SERVICE}" && test -d "./cmd/${SERVICE}"
RUN CGO_ENABLED=0 GOOS=linux GOFLAGS=-buildvcs=false \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/service ./cmd/${SERVICE}

FROM ${RUNTIME_IMAGE}

COPY --from=build /out/service /usr/local/bin/service

USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/service"]
