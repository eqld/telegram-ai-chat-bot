APP_NAME=telegram-ai-chat-bot
DOCKER_REPO=localhost
IMAGE_TAG=latest

.PHONY: run

run:
	go mod tidy
	go mod vendor
	docker build -t $(DOCKER_REPO)/$(APP_NAME):$(IMAGE_TAG) .
	docker run -it --rm \
		-e API_KEY_OPENAPI=$(API_KEY_OPENAPI) \
		-e API_KEY_TELEGRAM=$(API_KEY_TELEGRAM) \
		-e USER_ID_TELEGRAM=$(USER_ID_TELEGRAM) \
		-v $(CURDIR)/data:/data \
		$(DOCKER_REPO)/$(APP_NAME):$(IMAGE_TAG)
