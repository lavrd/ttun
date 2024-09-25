format:
	@go fmt main.go && \
		go fmt mock/custom/main.go

lint: format
	@go fmt ./...
	@golangci-lint run main.go && \
		golangci-lint run mock/custom/main.go

build:
	@./build.sh

build_docker: build
	docker build -t ttun -f Dockerfile .

test:
	go test -timeout 1m -v -count 1

run_docker_client:
	docker run --rm -it \
		--name ttun-client \
  	ttun "{\"side\":\"Client\"}"

run_docker_server:
	docker run --rm -it \
		--name ttun-server \
		-p 14600:14600 \
  	ttun "{\"side\":\"Server\"}"

curl_download_bytes:
	curl -LX GET 'http://127.0.0.1:14600/data/bytes.txt' -o mock.bytes.txt

curl_download_image:
	curl -LX GET 'http://127.0.0.1:14600/data/image.jpg' -o image.jpg
