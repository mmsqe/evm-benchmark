# syntax=docker/dockerfile:1.7
FROM golang:1.25-alpine AS evmd-builder

WORKDIR /src
RUN apk add --no-cache curl make git libc-dev bash build-base linux-headers eudev-dev

ARG COMMIT_SHA=main
RUN git clone --filter=blob:none --no-checkout "https://github.com/cosmos/evm" /src/app \
  && cd /src/app \
  && git fetch --depth=1 origin "$COMMIT_SHA" \
  && git checkout FETCH_HEAD

WORKDIR /src/app/evmd
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  go mod download

WORKDIR /src/app
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  make build

FROM alpine:3.20
RUN apk add --no-cache ca-certificates

COPY --from=evmd-builder /src/app/build/evmd /bin/evmd

# Common evmd ports: p2p, RPC, API, gRPC, pprof/json-rpc/metrics.
EXPOSE 26656 26657 1317 9090 26660 8545 8100 9464

# Keep CMD as default only, so benchmark runner can safely pass explicit command.
CMD ["/bin/evmd"]