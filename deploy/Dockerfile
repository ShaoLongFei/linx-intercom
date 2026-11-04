FROM golang:1.26-alpine AS build
WORKDIR /src
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/intercom-server ./cmd/intercom-server

FROM alpine:3.22
RUN addgroup -S intercom && adduser -S -G intercom intercom && apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/intercom-server /usr/local/bin/intercom-server
USER intercom
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/intercom-server"]
