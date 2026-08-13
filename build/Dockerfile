FROM debian:bookworm-slim

RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends -qq wget gcc libc6-dev ca-certificates && \
    rm -rf /var/lib/apt/lists/*

RUN wget -q --timeout=60 https://mirrors.aliyun.com/golang/go1.24.4.linux-amd64.tar.gz -O /tmp/go.tar.gz && \
    tar -C /usr/local -xzf /tmp/go.tar.gz && \
    rm /tmp/go.tar.gz

ENV PATH="${PATH}:/usr/local/go/bin"
ENV CGO_ENABLED=1
ENV GOPROXY=https://goproxy.cn,direct
ENV GOFLAGS=-mod=mod

WORKDIR /build

ENTRYPOINT ["sh", "-c", "CGO_ENABLED=1 go build -buildmode=c-shared -o /out/ark-429-autoban.so ./cmd/ark-429-autoban && echo BUILD_OK"]
