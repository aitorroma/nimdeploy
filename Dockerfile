# nimdeploy as a container: meant for the hub (nimdeploy hub serve).
# Agents run on the servers they deploy to, installed with get.sh.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" -o /out/nimdeploy .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/nimdeploy /usr/local/bin/nimdeploy
ENV NIMDEPLOY_HUB_LISTEN=0.0.0.0:9100
EXPOSE 9100
USER 65532:65532
HEALTHCHECK --interval=30s --timeout=6s --start-period=60s CMD ["nimdeploy", "hub", "healthcheck"]
ENTRYPOINT ["nimdeploy"]
CMD ["hub", "serve"]
