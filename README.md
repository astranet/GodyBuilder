## GodyBuilder

<img src="private/godybuilder.png" width="300px" />

A toolset for orchestrating Docker container builds for Go microservices. It should allow faster builds and can be used in watcher mode, that triggers the corresponding container reload upon source change.

### Docker images

* https://hub.docker.com/r/astranet/gody_build_alpine/
* https://hub.docker.com/r/astranet/gody_build_ubuntu/

and

* https://hub.docker.com/r/astranet/gody_run/

Image described by [/docker/Dockerfile.build_alpine](/docker/Dockerfile.build_alpine) is used for the building pipeline, given a list of packages, they will be built using an instance of this image, the state and build cache will be persisted for the whole process. If you want a clean state for each package, just run GodyBuilder multiple times. The image targets Alpine Linux. Another one is targeting the standard Ubuntu distributions.

Image described by [/docker/Dockerfile.run](/docker/Dockerfile.run) is used to run scratch containers, it uses Alpine Linux and should have all the required dependencies pre-installed, like CA certificates for SSL, or any Go dependency like Glide.

### Usage

```
$ GodyBuilder -h

Usage: GodyBuilder COMMAND [arg...]

Auto-compile daemon and parallel builder for Go executables targeting Docker environments.

Commands:
  build        Build all listed Go packages using shared state in a Docker container.
  watch        Start watching for changes in source code of packages and their deps.
  cleanup      Removes the builder container completely.

Run 'GodyBuilder COMMAND --help' for more information on a command.

$ GodyBuilder build -h

Usage: GodyBuilder build [OPTIONS]

Build all listed Go packages using shared state in a Docker container.

Options:
  -v, --verbose    Verbosive logging.
  -p, --pkg        Adds packages into list to build.
  -i, --image      The builder container image to use (gody_build_alpine or gody_build_ubuntu). (default "astranet/gody_build_alpine")
  -n, --name       Override the builder container name. (default "gody_builder_1")
  -o, --out        Output directory for executable artifacts. (default "bin/")
  -j, --jobs       Number of parallel build jobs. (default 2)
  -f, --fancy      Enables dynamic reporting. (default true)
      --progress   Enables progress bar.
```

To build an executable package using Ubuntu image:

```
$ GodyBuilder -i gody_build_ubuntu -p github.com/astranet/galaxy/cmd/galaxy
$ GodyBuilder cleanup # wipe caches and docker images
```

### Troubleshooting

```
Error response from daemon: client version 1.40 is too new. Maximum supported API version is 1.39
```

To fix the error use the following:

```
export DOCKER_API_VERSION=1.39
```

### License

[MIT](/LICENSE)
