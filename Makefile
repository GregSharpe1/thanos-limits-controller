
CONTAINER_TAG ?= "test"

# Application arguments
ACTIVE_SERIES_MAX ?= 5
CONFIGMAP_NAME ?= thanos-receive-limits
CONFIGMAP_GENERATED_NAME ?= thanos-receive-limits-generated
STATEFULSET_LABEL ?= controller.limits.thanos.io="true"

# Metrics
METRICS_PORT ?= "9096"
METRICS_PATH ?= "/metrics"

export LOG_LEVEL=debug
export NAMESPACE=default

container-build:
	docker build -t $(CONTAINER_TAG) .

container-run:
	printenv
	docker run \
		--network=host \
		-e LOG_LEVEL="debug" \
		-v $$HOME/.kube:/app/.kube \
		test \
		./thanos-limits-controller \
		-configmap-name=$(CONFIGMAP_NAME) \
		-configmap-generated-name=$(CONFIGMAP_GENERATED_NAME) \
		-statefulset-label $(STATEFULSET_LABEL) \
		-active-series-max=$(ACTIVE_SERIES_MAX)

build:
	CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o ./thanos-limits-controller .

run:
	go run ./main.go \
		-configmap-name $(CONFIGMAP_NAME) \
		-configmap-generated-name $(CONFIGMAP_GENERATED_NAME) \
		-statefulset-label $(STATEFULSET_LABEL) \
		-active-series-max $(ACTIVE_SERIES_MAX) \
		-interval 10s \
		-metrics-port $(METRICS_PORT) \
		-metrics-path $(METRICS_PATH)
