FROM golang:1.23.1-alpine3.20
WORKDIR /mock-server
COPY go.mod .
COPY main.go .
RUN go build -o app
ENTRYPOINT ["/mock-server/app"]
