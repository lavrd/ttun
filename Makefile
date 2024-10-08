test_data_folder = test-data

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
	go test . -timeout 1m -v -count 1

run_docker_client:
	docker run --rm -it \
  	ttun client

run_docker_server:
	docker run --rm -it \
		--name ttun-server \
  	ttun --json-logs server

run_caddy:
	docker run -it --rm --name ttun-caddy -p 15800:15800 caddy:2.8.4-alpine \
		caddy reverse-proxy \
		--from http://localhost:15800 \
		--to 172.17.0.3:14600

curl_download_bytes:
	mkdir -p $(test_data_folder)
	curl -LX GET 'http://localhost:15800/data/bytes.txt' -o $(test_data_folder)/mock.bytes.txt

curl_download_image:
	mkdir -p $(test_data_folder)
	curl -LX GET 'http://localhost:15800/data/image.jpg' -o $(test_data_folder)/image.jpg
