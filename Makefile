SHELL := /bin/sh

GATEWAY_IMAGE := halon/gateway:dev
AGENT_IMAGE := halon/agent:dev
DEMO_IMAGE := halon/user-service-demo:dev
GO_BIN := $(shell go env GOPATH)/bin
export PATH := $(GO_BIN):$(PATH)

.PHONY: test run-gateway run-agent run-demo build build-demo docker-build docker-build-demo minikube-start minikube-image deploy status clean

test:
	go test -race ./...
	go vet ./...

run-gateway:
	go run ./cmd/gateway

run-agent:
	go run ./cmd/agent

run-demo:
	go run ./examples/user-service

build:
	CGO_ENABLED=0 go build -trimpath -o bin/gateway ./cmd/gateway
	CGO_ENABLED=0 go build -trimpath -o bin/agent ./cmd/agent

build-demo:
	CGO_ENABLED=0 go build -trimpath -o bin/user-service-demo ./examples/user-service

docker-build:
	docker build --build-arg SERVICE_PATH=cmd/gateway -t $(GATEWAY_IMAGE) .
	docker build --build-arg SERVICE_PATH=cmd/agent -t $(AGENT_IMAGE) .

docker-build-demo:
	docker build --build-arg SERVICE_PATH=examples/user-service -t $(DEMO_IMAGE) .

minikube-start:
	minikube start --driver=docker

# Optional convenience for local Minikube development. Any other cluster
# works too: push the images to your registry and point the manifests at it.
minikube-image: docker-build docker-build-demo
	minikube image load $(GATEWAY_IMAGE) $(AGENT_IMAGE) $(DEMO_IMAGE)

deploy:
	kubectl apply -f deploy/kubernetes/

status:
	kubectl get pods,services,statefulsets -l app.kubernetes.io/part-of=halon

clean:
	rm -rf bin coverage.out
