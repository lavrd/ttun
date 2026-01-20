test_data_folder = test-data
port = 15800

format:
	@go fmt main.go && \
		go fmt mock/custom/main.go

lint: format
	@go fmt ./...
	@golangci-lint run main.go && \
		golangci-lint run mock/custom/main.go

build_docker:
	docker build -t ttun -f Dockerfile .

build_docker_race:
	docker build -t ttun -f Dockerfile . --build-arg race=1

test:
	go test . -timeout 1m -v -count 1 -race

run_docker_client:
	docker run --rm -it ttun client

run_docker_server:
	docker run --rm -it --name ttun-server -p 14600:14600 ttun --json-logs server

run_caddy:
	docker run -it --rm --name ttun-caddy -p 15800:15800 caddy:2.10.2-alpine \
		caddy reverse-proxy \
		--from http://localhost:15800 \
		--to 172.17.0.3:14600

run_nginx:
	docker run -it --rm --name ttun-nginx \
		-p 15800:15800 \
		-v "$(PWD)/nginx.conf:/etc/nginx/nginx.conf:ro" \
		nginx:1.29.4-alpine3.23

curl_download_bytes:
	mkdir -p $(test_data_folder)
	curl -LX GET 'http://localhost:$(port)/data/bytes.txt' -o $(test_data_folder)/mock.bytes.txt

curl_download_image:
	mkdir -p $(test_data_folder)
	curl -LX GET 'http://localhost:$(port)/data/image.jpg' -o $(test_data_folder)/image.jpg
