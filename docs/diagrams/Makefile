IMAGE_NAME := diagrams

.PHONY: runlocal devlocal

build: runlocal/gitea_controller.py runlocal/profiles_controller_overview.py runlocal/blobcsi_system.py

runlocal/%:
	echo "Running command: python $(notdir $@)"
	python $(notdir $@)

devlocal/%:
	@echo "Watching $(notdir $@) for changes"
	inotifywait -m $(notdir $@) -e modify | while read path action file; do \
		make runlocal/$(notdir $@); \
	done;
