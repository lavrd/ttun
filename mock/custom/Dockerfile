FROM golang:1.25.6-alpine3.23 AS builder
WORKDIR /mock-custom

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY main.go .
RUN go build -o app


FROM alpine:3.23 AS app
COPY --from=builder /mock-custom/app /mock-custom/app
ENTRYPOINT ["/mock-custom/app"]
