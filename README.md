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

```shell
# Run server with mock data.
cd mock/custom && make run_docker
# Build docker image with client and server.
make build_docker
# Run server.
make run_docker_server
# Run client.
make run_docker_client
# Run Caddy.
make run_caddy
# Try to download some data through tunnel.
make curl_download_bytes
# Or.
make curl_download_image
# You can find downloaded files in ./test-data folder.
```

To make this demo working automatically you should stop all containers before starting it.

Why we need Caddy? - as ttun doesn't support HTTPS and we want to use Caddy for it.
