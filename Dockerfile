# The image holds one file, /drover, a static binary. Other images take it with:
#   COPY --from=ghcr.io/mrofi/drover:latest /drover /usr/local/bin/drover
# The build stage runs on the builder's own architecture and cross-compiles for the target one.
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go llms.txt ./
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /drover .

FROM scratch
COPY --from=build /drover /drover
USER 65532:65532
ENTRYPOINT ["/drover"]
CMD ["run"]
