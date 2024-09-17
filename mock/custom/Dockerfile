FROM golang:1.23.1-alpine3.20
WORKDIR /mock-custom
COPY go.mod .
COPY main.go .
RUN go build -o app
ENTRYPOINT ["/mock-custom/app"]
