build_docker:
	@docker build -t mock-custom -f Dockerfile .

run_docker: build_docker
	@docker run -it --rm \
		--name mock-custom \
		-p 12300:44000 \
		-v "${PWD}/../data/:/data/" \
		mock-custom server

gen_mock_bytes:
	@go run . gen
