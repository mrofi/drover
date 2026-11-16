# Adding drover to an image that already has an app: two lines and a CMD.
# Build from the repository root:  docker build -f examples/inject/Dockerfile -t drover-inject .
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /drover . && CGO_ENABLED=0 go build -o /app ./testdata/fakeapp

# Stand-in for "your image".
FROM alpine:3.20
COPY --from=build /app /usr/local/bin/app
# In your own Dockerfile this is: COPY --from=ghcr.io/mrofi/drover:latest /drover /usr/local/bin/drover
COPY --from=build /drover /usr/local/bin/drover

# No root needed: drover only writes to its data directory.
USER 1000:1000
ENV DROVER_DATA=/tmp/drover
EXPOSE 8080
CMD ["drover", "run", "--listen", ":8080", "--instances", "3", "--health", "/health", "--", "app", "--port", "{port}"]
