FROM alpine:3.20
COPY zig-out/linux/bin/ttun /app
ENTRYPOINT ["/app"]
