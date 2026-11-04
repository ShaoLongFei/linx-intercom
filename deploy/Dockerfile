FROM golang:1.26-bookworm AS build
WORKDIR /src
ARG TARGETARCH
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/intercom-server ./cmd/intercom-server \
    && case "$TARGETARCH" in \
         amd64) sherpa_arch=x86_64-unknown-linux-gnu ;; \
         arm64) sherpa_arch=aarch64-unknown-linux-gnu ;; \
         *) echo "unsupported sherpa-onnx architecture: $TARGETARCH" >&2; exit 1 ;; \
       esac \
    && mkdir -p /out/sherpa \
    && cp "$(go env GOPATH)/pkg/mod/github.com/k2-fsa/sherpa-onnx-go-linux@v1.13.8/lib/$sherpa_arch/"*.so /out/sherpa/

FROM debian:bookworm-slim
RUN groupadd --system intercom && useradd --system --gid intercom intercom && apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libgomp1 libstdc++6 \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /var/lib/intercom \
    && chown intercom:intercom /var/lib/intercom
WORKDIR /app
COPY --from=build /out/intercom-server /usr/local/bin/intercom-server
COPY --from=build /out/sherpa /opt/sherpa
ENV LD_LIBRARY_PATH=/opt/sherpa
USER intercom
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/intercom-server"]
