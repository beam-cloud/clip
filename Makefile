imageVersion := latest

build:
	okteto build --build-arg BUILD_ENV=okteto -f ./Dockerfile -t okteto.dev/beam-clip:$(imageVersion)

start:
	okteto up --file okteto.yml

stop:
	okteto down --file cacher.yml