FROM ubuntu:latest
MAINTAINER wangrongxiang

RUN apt-get update && \
    apt-get upgrade && \
    apt-get install -y curl sudo && \
    sudo apt-get install -y ssh libevent-dev libcurl4-openssl-dev libglib2.0-dev uuid-dev intltool && \
    sudo apt-get install -y libsqlite3-dev libmysqlclient-dev libarchive-dev libtool libjansson-dev valac libfuse-dev && \
    sudo apt-get install -y cmake re2c flex sqlite3 python-pip git libssl-dev libldap2-dev libonig-dev vim vim-scripts && \
    sudo apt-get install -y wget gcc autoconf automake mysql-client librados-dev libxml2-dev curl telnet && \
    sudo apt-get install -y netcat unzip netbase ca-certificates apt-transport-https build-essential libxslt1-dev libffi-dev && \
    sudo apt-get install -y libpcre3-dev zlib1g-dev xz-utils nginx pkg-config poppler-utils libmemcached-dev libjwt-dev

WORKDIR /root/dev/source-code
COPY . /root/dev/source-code/seafile-server
COPY ./config_files /root/dev
RUN git clone https://github.com/haiwen/libevhtp.git /root/dev/source-code/libevhtp && \
    git clone https://github.com/haiwen/libsearpc.git /root/dev/source-code/libsearpc && \
    git clone https://github.com/lovehunter9/seahub.git /root/dev/source-code/seahub

WORKDIR libevhtp
RUN cmake -DEVHTP_DISABLE_SSL=ON -DEVHTP_BUILD_SHARED=OFF . && make && sudo make install && sudo ldconfig

WORKDIR ../libsearpc
RUN ./autogen.sh && ./configure && make && sudo make install && sudo ldconfig

WORKDIR ../seafile-server
RUN ./autogen.sh && ./configure --disable-fuse && make && sudo make install && sudo ldconfig

EXPOSE 8082

ENTRYPOINT ["seaf-server", "-c", "/root/dev/conf", "-d", "/root/dev/seafile-data", "-D", "all", "-f", "-l", "-", "&"]
