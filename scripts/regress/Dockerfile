# Mora Tier-1 regression harness — a clean Linux box that has never seen Go or a
# prior mora install. Build context is assembled by build.sh and contains:
#   repo/            the mora source (for go build, install.sh, scripts, world.json)
#   regression.sh    the Tier-1 harness
#
#   docker build --build-arg VER=x.y.z -t mora-regress <context>
#   docker run --rm --tmpfs /work mora-regress

# ---- build: stamped, CGO=0, mirrors the release ldflags --------------------
FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY repo/ /src/
ARG VER=0.0.0-regress
ARG SHA=local
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VER} -X main.commit=${SHA} -X main.date=docker" \
      -o /out/mora ./cmd/mora

# ---- harness: thin runtime, proves the single-static-binary thesis ----------
FROM debian:bookworm-slim AS harness
RUN apt-get update && apt-get install -y --no-install-recommends \
      curl ca-certificates tar git python3 \
    && rm -rf /var/lib/apt/lists/*
COPY repo/ /repo/
COPY --from=build /out/mora /opt/mora-head
COPY regression.sh /regression.sh
ARG VER=0.0.0-regress
ENV MORA_REPO=/repo \
    MORA_BIN=/opt/mora-head \
    EXPECTED_VER=${VER} \
    WORK=/work
ENTRYPOINT ["bash", "/regression.sh"]
