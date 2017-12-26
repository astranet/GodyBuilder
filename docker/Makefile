all: build-image run-image run-image-push run-image-push

build-image:
	docker build --force-rm -t bringhub/gody_build -f Dockerfile.build .

run-image:
	docker build --force-rm -t bringhub/gody_run -f Dockerfile.run .

build-image-push:
	docker push bringhub/gody_build

run-image-push:
	docker push bringhub/gody_run
