## GodyBuilder

A toolset for orchestrating Docker container builds for Go microservices. It should allow faster builds and can be used in watcher mode, that triggers the corresponding container reload upon source change.

### Two Docker images

* https://cloud.docker.com/app/bringhub/repository/docker/bringhub/gody_build/general
* https://cloud.docker.com/app/bringhub/repository/docker/bringhub/gody_run/general

Image described by [/docker/Dockerfile.build](/docker/Dockerfile.build) is used for the building pipeline, given a list of packages, they will be built using an instance of this image, the state and build cache will be persisted for the whole process. If you want a clean state for each package, just run GodyBuilder multiple times. The image targets Alpine Linux.

Image described by [/docker/Dockerfile.run](/docker/Dockerfile.run) is used to run scratch containers, it uses Alpine Linux and should have all the required dependencies pre-installed, like CA certificates for SSL,
or any Go dependency like Glide.

### Usage

TODO

### License

[MIT](/LICENSE)
