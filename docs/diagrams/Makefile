IMAGE_NAME := diagrams

.PHONY: runlocal devlocal

run: gitea_controller.png profiles_controller_overview.png blobcsi_controller.png

runlocal/%:
	echo "Running command: python $(notdir $@)"
	python $(notdir $@)

devlocal/%:
	@echo "Watching $(notdir $@) for changes"
	inotifywait -m $(notdir $@) -e modify | while read path action file; do \
		make runlocal/$(notdir $@); \
	done;

%.png: %.py build
	docker run -v $$(pwd)/:/tmp/ $(IMAGE_NAME) $<

build: Dockerfile
	docker build . -t $(IMAGE_NAME)
