SHELL := /bin/sh

USER_IMAGE := halon/user-service:dev
GATEWAY_IMAGE := halon/gateway:dev
GO_BIN := $(shell go env GOPATH)/bin
export PATH := $(GO_BIN):$(PATH)

.PHONY: proto generate test run run-gateway build docker-build minikube-start minikube-image deploy status clean

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

run-gateway:
	go run ./cmd/gateway

build:
	CGO_ENABLED=0 go build -trimpath -o bin/user-service ./cmd/user-service
	CGO_ENABLED=0 go build -trimpath -o bin/gateway ./cmd/gateway

docker-build:
	docker build --build-arg SERVICE=user-service -t $(USER_IMAGE) .
	docker build --build-arg SERVICE=gateway -t $(GATEWAY_IMAGE) .

minikube-start:
	minikube start --driver=docker

minikube-image: docker-build
	minikube image load $(USER_IMAGE) $(GATEWAY_IMAGE)

deploy:
	kubectl apply -f deploy/kubernetes/

status:
	kubectl get pods,services,statefulsets -l app.kubernetes.io/part-of=halon

clean:
	rm -rf bin coverage.out
