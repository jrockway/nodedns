FROM golang:1.16 AS build
WORKDIR /nodedns
COPY go.mod go.sum /nodedns/
RUN go mod download

COPY . /nodedns/
RUN CGO_ENABLED=0 go install ./cmd/nodedns

FROM gcr.io/distroless/static-debian10
COPY --from=build /go/bin/nodedns /go/bin/nodedns
CMD ["/go/bin/nodedns"]
