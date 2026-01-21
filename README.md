# ttun

Simple transport layer tunnel which can be used as a reverse proxy to serve your local traffic (localhost) publicity.

## Usage

```shell
Usage: ttun <command> [flags]

Flags:
  -h, --help         Show context-sensitive help.
      --json-logs    Print logs in json.

Commands:
  client [flags]
    Start client side.

  server [flags]
    Start server side.

Run "ttun <command> --help" for more information on a command.
```

### Demo with mock server

Docker should be up and running before executing commands below.

To make this demo working automatically you should stop all containers before starting it.

```shell
# Generate mock data.
cd mock/custom && go run . gen

# Run server with mock data.
# You can use custom server (Go).
cd mock/custom && make run_docker
# Or run Nginx to serve mock data.
cd mock/nginx && make build_docker && make run_docker

# Build docker image with client and server.
make build_docker

# Run server.
make run_docker_server
# Run client.
make run_docker_client

# Now we need to run reverse proxy.
# You can use Caddy.
make run_caddy
# Or Nginx.
make run_nginx

# So now we can try to download data through tunnel.
# Raw bytes.
make curl_download_bytes
# Or image.
make curl_download_image

# You can find downloaded files in ./test-data folder.
```
