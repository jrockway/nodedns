FROM golang:1.13-alpine AS build
RUN apk add git bzr gcc musl-dev
WORKDIR /nodedns
COPY go.mod go.sum /nodedns/
RUN go mod download

COPY . /nodedns/
RUN go install ./cmd/nodedns

FROM alpine:latest
RUN apk add ca-certificates tzdata
WORKDIR /
RUN wget -O /usr/local/bin/dumb-init https://github.com/Yelp/dumb-init/releases/download/v1.2.2/dumb-init_1.2.2_x86_64
RUN chmod +x /usr/local/bin/dumb-init
ENTRYPOINT ["/usr/local/bin/dumb-init"]
COPY --from=build /go/bin/nodedns /go/bin/nodedns
CMD ["/go/bin/nodedns"]
