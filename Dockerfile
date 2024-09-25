FROM alpine:3.20
COPY target/ttun /ttun
ENTRYPOINT ["/ttun"]
