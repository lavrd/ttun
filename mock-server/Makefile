format:
	@go fmt main.go

lint:
	@go fmt ./...
	@golangci-lint run main.go

build_docker:
	docker build -t mock-server -f Dockerfile .

run_docker: build_docker
	docker run -it --rm --name mock-server mock-server
