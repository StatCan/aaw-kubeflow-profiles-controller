IMAGE_NAME := diagrams

run: gitea_controller.png

%.png: %.py build
	docker run -v $$(pwd)/:/tmp/ $(IMAGE_NAME) $<

build: Dockerfile
	docker build . -t $(IMAGE_NAME)