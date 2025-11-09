SHELL := /bin/bash

# tweak if you change your module path or want an image tag
IMAGE_CI     ?= cf-origin-pl-ci:local
IMAGE_SMOKE  ?= cf-origin-pl-smoke:local
GO_VERSION   ?= 1.25
MODULE_PATH  ?= github.com/NebulaRover77/cf-origin-pl

.PHONY: docker-ci docker-test smoke clean

# Build & run tests inside Docker (fails the build if tests fail)
docker-ci:
	@docker build \
	  --target ci \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  -t $(IMAGE_CI) -f Dockerfile .

# (no-op run; tests already executed at build time)
docker-test:
	@docker inspect $(IMAGE_CI) >/dev/null
	@echo "✅ docker image present (tests ran during docker build)"

smoke:
	@docker build \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  --build-arg MOD=$(MODULE_PATH) \
	  -t $(IMAGE_SMOKE) -f Dockerfile.smoke .
	@docker run --rm $(IMAGE_SMOKE) caddy list-modules | grep -q cloudfront_origin_pl && \
	  echo "✅ module is registered in Caddy (smoke OK)"

clean:
	@docker rmi $(IMAGE_CI) $(IMAGE_SMOKE) >/dev/null 2>&1 || true
	@echo "🧹 cleaned images (if present)"
