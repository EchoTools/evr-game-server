# build

FROM golang:1.21.6 as builder

COPY ./main.go /go/src/.
COPY ./go.mod /go/src/.
COPY ./go.sum /go/src/.
COPY ./vendor /go/src/.

WORKDIR /go/src
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o wrapper .

# final image
FROM debian:bullseye-slim AS winebase

ENV DEBIAN_FRONTEND="noninteractive"

RUN dpkg --add-architecture i386 && \
    apt-get update -y && \
    apt-get install -y --no-install-recommends \
	    apt-utils \
        lsb-release \
	    software-properties-common \
        ca-certificates \
        locales \
        wget \
        gnupg2 \
        cabextract \
        procps && \
    mkdir -pm755 /etc/apt/keyrings && \
    wget -O /etc/apt/keyrings/winehq-archive.key https://dl.winehq.org/wine-builds/winehq.key && \
    wget -NP /etc/apt/sources.list.d/ https://dl.winehq.org/wine-builds/debian/dists/bullseye/winehq-$(lsb_release -sc).sources && \
    apt-add-repository -y "deb http://deb.debian.org/debian $(lsb_release -sc) main contrib" && \
    apt-get update -y && \
    apt-get install -y --install-recommends \
        wine \
        wine32 \
        wine64 \
        libwine \
        libwine:i386 \
        fonts-wine \
        winetricks \ 
        winbind  && \
    apt-get -y autoremove && \
    rm -rf \
        /tmp/* \
        /var/tmp/* \
        /var/lib/apt/lists/*


ENV DATAPATH="/data"
ENV CONFIGPATH="/data/config.json"
ENV LOGPATH="/data/log/echovr.log"
ENV GAMEDATAPATH="/data/game"
ENV APPDATAPATH="/data/app"


VOLUME /echovr
COPY --link ./echovr/. /echovr/.

VOLUME /data
COPY ./data/. data/.

# Setup the application data volume
RUN mkdir -p /root/.wine/drive_c/users/root/Local\ Settings/Application\ Data && \
    ln -sf {$APPDATAPATH} /root/.wine/drive_c/users/root/Local\ Settings/Application\ Data/rad

RUN rm -f /echovr/_local/config.json && ln -s ${CONFIGPATH} /echovr/_local/config.json

RUN rm -rf /echovr/sourcedb/rad15/json/r14 && \
    ln -s ${GAMEDATAPATH} /echovr/sourcedb/rad15/json/r14

ENV BCASTPORT=6792
ENV HTTPAPIPORT=6721


ENV WINEDEBUG=-all
ENV TERM=xterm

#USER nobody:nogroup

RUN winetricks -q winhttp && \
    wine wineboot -s

COPY --from=builder /go/src/wrapper /wrapper

ENTRYPOINT ["/wrapper"]
