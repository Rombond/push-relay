FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /relay ./cmd/relay
RUN mkdir /data-empty

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /relay /relay
COPY --from=build --chown=65532:65532 /data-empty /data
ENV RELAY_CONFIG=/config/relay.yaml
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/relay"]
CMD ["serve"]
