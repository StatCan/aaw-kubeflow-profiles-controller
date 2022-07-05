IMAGE_NAME := diagrams

.PHONY: generate-pngs

run: gitea_controller.png profiles_controller_overview.png blobcsi_controller.png

runlocal/%:
	echo "Running command: python $(notdir $@)"
	python $(notdir $@)

%.png: %.py build
	docker run -v $$(pwd)/:/tmp/ $(IMAGE_NAME) $<

build: Dockerfile
	docker build . -t $(IMAGE_NAME)
