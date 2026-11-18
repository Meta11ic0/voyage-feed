FROM golang:1.25.14-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY dataset ./dataset
COPY engine ./engine
COPY store ./store
COPY httpapi ./httpapi
COPY stream ./stream
COPY data/dev.json ./data/dev.json
COPY data/scale.json ./data/scale.json
RUN CGO_ENABLED=0 go build -o /out/market-simulator ./cmd/market-simulator

FROM build AS test
COPY data/dev-src ./data/dev-src
COPY data/dev.report.json ./data/dev.report.json
RUN go vet ./... && go test -race ./...

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates passwd \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --home-dir /app --shell /usr/sbin/nologin app \
    && mkdir -p /app \
    && chown 65532:65532 /app
COPY --from=build /out/market-simulator /app/market-simulator
COPY --from=build --chown=65532:65532 /src/data/dev.json /app/data/dev.json
COPY --from=build --chown=65532:65532 /src/data/scale.json /app/data/scale.json
USER 65532
ENTRYPOINT ["/app/market-simulator"]
