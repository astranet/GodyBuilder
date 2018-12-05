all: build-images run-image build-images-push run-image-push

build-images:
	docker build --force-rm -t astranet/gody_build_alpine -f Dockerfile.build_alpine .
	docker build --force-rm -t astranet/gody_build_ubuntu -f Dockerfile.build_ubuntu .

run-image:
	docker build --force-rm -t astranet/gody_run -f Dockerfile.run .

build-images-push:
	docker push astranet/gody_build_alpine
	docker push astranet/gody_build_ubuntu

run-image-push:
	docker push astranet/gody_run
