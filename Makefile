SHELL := /bin/sh

APP := user-service
IMAGE := halon/$(APP):dev
GO_BIN := $(shell go env GOPATH)/bin
export PATH := $(GO_BIN):$(PATH)

.PHONY: proto generate test run build docker-build minikube-start minikube-image deploy status clean

proto generate:
	mkdir -p api/gen/user/v1
	protoc -I api/proto \
		--go_out=api/gen --go_opt=paths=source_relative \
		--go-grpc_out=api/gen --go-grpc_opt=paths=source_relative \
		api/proto/user/v1/user.proto

test:
	go test -race ./...
	go vet ./...

run:
	go run ./cmd/user-service

build:
	CGO_ENABLED=0 go build -trimpath -o bin/$(APP) ./cmd/user-service

docker-build:
	docker build -t $(IMAGE) .

minikube-start:
	minikube start --driver=docker

minikube-image: docker-build
	minikube image load $(IMAGE)

deploy:
	kubectl apply -f deploy/kubernetes/

status:
	kubectl get pods,services -l app.kubernetes.io/name=user-service

clean:
	rm -rf bin coverage.out
