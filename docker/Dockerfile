FROM ubuntu:18.04 AS build
RUN apt update && \
    apt install -y --no-install-recommends libaio-dev libleveldb-dev libsnappy-dev \
    g++ libcap2-bin libseccomp-dev golang git ca-certificates make sudo
RUN apt install -y --no-install-recommends jq openssl
ENV GOPATH=/stenographer
RUN go get github.com/google/stenographer
ENV BINDIR=/opt/stenographer/bin
RUN mkdir -p ${BINDIR} && \
    cd /stenographer/src/github.com/google/stenographer && \
    sed -i -e 's|/path/to|/var/lib/stenographer|' configs/steno.conf && \
    ( ./install.sh || true ) && \
    sed -i -e 's/curl /curl -k /' /opt/stenographer/bin/stenocurl

FROM ubuntu:18.04
COPY --from=build /opt/stenographer/bin /usr/bin
COPY --from=build /etc/stenographer /etc/stenographer
COPY --from=build /stenographer/src/github.com/google/stenographer/stenotype/compile_bpf.sh /usr/bin/
COPY entrypoint.sh /usr/bin/

RUN adduser --system --no-create-home stenographer && \
    addgroup --system stenographer && \
    mkdir -p /var/lib/stenographer && \
    chown stenographer:stenographer /var/lib/stenographer && \
    apt update && \
    apt install -y --no-install-recommends libleveldb1v5 libsnappy1v5 libaio1 \
    jq tcpdump libcap2-bin curl tshark ca-certificates sudo && \
    setcap 'CAP_NET_RAW+ep CAP_NET_ADMIN+ep CAP_IPC_LOCK+ep' /usr/bin/stenotype && \
    rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deb.nodesource.com/setup_14.x | sudo -E bash - && \
    sudo apt-get install -y nodejs && \
    rm -rf /var/lib/apt/lists/* && \
    npm install -g stenoread

ENTRYPOINT [ "/usr/bin/entrypoint.sh" ]
