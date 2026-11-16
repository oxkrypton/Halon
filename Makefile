SHELL := /bin/sh

GATEWAY_IMAGE := halon/gateway:dev
AGENT_IMAGE := halon/agent:dev
DEMO_USER_IMAGE := halon/user-service-demo:dev
DEMO_ORDER_IMAGE := halon/order-service-demo:dev
GO_BIN := $(shell go env GOPATH)/bin
export PATH := $(GO_BIN):$(PATH)

.PHONY: test lint run-gateway run-agent run-demo build build-demo docker-build docker-build-demo minikube-start minikube-image deploy deploy-observability deploy-redis status clean

test:
	go test -race ./...
	go vet ./...

lint:
	golangci-lint run ./...

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
	CGO_ENABLED=0 go build -trimpath -o bin/order-service-demo ./examples/order-service

docker-build:
	docker build --build-arg SERVICE_PATH=cmd/gateway -t $(GATEWAY_IMAGE) .
	docker build --build-arg SERVICE_PATH=cmd/agent -t $(AGENT_IMAGE) .

docker-build-demo:
	docker build --build-arg SERVICE_PATH=examples/user-service -t $(DEMO_USER_IMAGE) .
	docker build --build-arg SERVICE_PATH=examples/order-service -t $(DEMO_ORDER_IMAGE) .

minikube-start:
	minikube start --driver=docker

# Optional convenience for local Minikube development. Any other cluster
# works too: push the images to your registry and point the manifests at it.
minikube-image: docker-build docker-build-demo
	minikube image load $(GATEWAY_IMAGE) $(AGENT_IMAGE) $(DEMO_USER_IMAGE) $(DEMO_ORDER_IMAGE)

deploy:
	kubectl apply -f deploy/kubernetes/

# Optional observability reference stack (Prometheus/Grafana/Loki/Tempo/
# OTel Collector). Not part of `make deploy`; see docs/deployment.md.
deploy-observability:
	kubectl apply -f deploy/kubernetes/observability/

# Optional shared-state Redis reference manifest. Not part of `make deploy`;
# see docs/deployment.md for managed/HA production guidance.
deploy-redis:
	kubectl apply -f deploy/kubernetes/redis/

status:
	kubectl get pods,services,statefulsets -l app.kubernetes.io/part-of=halon

clean:
	rm -rf bin coverage.out
