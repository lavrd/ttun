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

run_client: build_docker
	docker run --rm -it \
		--name ttun-client \
  	ttun

run_server: build_docker
	docker run --rm -it \
		--name ttun-server \
		-p 14600:14600 \
  	ttun
