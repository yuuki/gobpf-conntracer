FROM ubuntu:20.10 as builder
RUN apt-get -y update && apt-get -y install --no-install-recommends --fix-missing \
    libz-dev libelf-dev llvm clang  \
    make wget ca-certificates build-essential gcc sudo \
    && apt-get purge --auto-remove && apt-get clean
ENV GOVER '1.15.5'
ENV GOTAR "go${GOVER}.linux-amd64.tar.gz"
RUN wget https://dl.google.com/go/${GOTAR} \
    && tar -C /usr/local -xzf ${GOTAR} \ 
    && rm -f ${GOTAR}
ENV PATH $PATH:/usr/local/go/bin

FROM builder as build
COPY . /conntop
RUN make conntop

FROM alpine as runtime
RUN apk --no-cache update && apk --no-cache add libc6-compat elfutils-dev zlib-static

WORKDIR /conntop

FROM runtime
WORKDIR /conntop
COPY --from=build /conntop/conntop ./
ENTRYPOINT ["./conntop"]
