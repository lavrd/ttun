build_docker:
	@docker build -t mock-nginx -f Dockerfile .

run_docker: build_docker
	@docker run -it --rm \
		--name mock-nginx \
		-v "${PWD}/../data/:/usr/share/nginx/html/data" \
		-p 15700:80 \
		mock-nginx
