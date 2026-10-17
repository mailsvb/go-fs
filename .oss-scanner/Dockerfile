# The image Anthropic's OSS Scanner builds go-fs in (github.com/anthropics/oss-scanner,
# projects/go-fs). The build runs with network access; the scan after it runs
# without, so everything the tests and new reproducers need is fetched here.
FROM golang:1.27.2-trixie

ENV DEBIAN_FRONTEND=noninteractive \
    CGO_ENABLED=0 \
    GOTOOLCHAIN=local

# rpm brings rpmkeys, with which internal/rpm checks the signatures it makes;
# without it that test skips. jarsigner, which internal/fdroid uses the same
# way, would take a JDK and is left out.
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates git build-essential rpm procps \
    && rm -rf /var/lib/apt/lists/*

# scanner contract: the checkout lives inside the image, at /src
COPY . /src
WORKDIR /src

RUN go mod download

# Both products, as the Makefile builds them but not stripped: go-fs, and
# go-fs-light, which leaves out every protocol but HTTP, HTTPS, S3 and the
# registry.
RUN go build -trimpath -o /usr/local/bin/go-fs . \
    && go build -trimpath -tags light -o /usr/local/bin/go-fs-light .

# Compile every test binary, the fuzz targets with them, so that reproducers
# build offline, then run the suite once as a baseline without failing the
# build on it. internal/httpd starts many servers and gets flaky when several
# test binaries run at once, hence -p 2.
RUN go vet ./... && go vet -tags light ./... \
    && go test -count=1 -run '^$' ./... >/dev/null
RUN go test -count=1 -p 2 -timeout 20m ./... \
    || echo 'WARNING: go test reported failures'

# New reproducers and patches must build with the network gone.
ENV GOPROXY=off \
    GOSUMDB=off

CMD ["bash"]
