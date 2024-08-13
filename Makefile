format:
	zig fmt src/main.zig

build: format
	zig build --summary all

build_linux: format
	mkdir -p zig-out/linux/bin
	./build.sh

build_docker: build_linux
	docker build -t ttun -f Dockerfile .

test: format
	zig test src/main.zig

test_one: format
	zig test src/main.zig --test-filter $(name)

run_docker_client: build_docker
	docker run --rm -it \
		--name ttun-client \
  	ttun "{\"side\":\"Client\"}"

run_docker_server: build_docker
	docker run --rm -it \
		--name ttun-server \
		-p 14600:14600 \
  	ttun "{\"side\":\"Server\"}"

curl_download:
	curl -iLX GET 'http://127.0.0.1:14600/data/bytes.txt' -o mock.bytes.txt
