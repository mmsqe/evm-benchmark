FROM golang:1.25-alpine AS evmd-builder

ARG COMMIT_SHA=main

WORKDIR /src
RUN apk add --no-cache curl make git libc-dev bash build-base linux-headers eudev-dev

RUN git clone "https://github.com/cosmos/evm" /src/app \
  && cd /src/app \
  && git checkout "$COMMIT_SHA"

WORKDIR /src/app/evmd
RUN go mod tidy

WORKDIR /src/app
RUN make build

FROM alpine:3.20
RUN apk add --no-cache ca-certificates

COPY --from=evmd-builder /src/app/build/evmd /bin/evmd

# Common evmd ports: p2p, RPC, API, gRPC, pprof/json-rpc/metrics.
EXPOSE 26656 26657 1317 9090 26660 8545 8100 9464

# Keep CMD as default only, so benchmark runner can safely pass explicit command.
CMD ["/bin/evmd"]