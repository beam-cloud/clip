imageVersion := latest

build:
	okteto build --build-arg BUILD_ENV=okteto -f ./Dockerfile -t okteto.dev/beam-clip:$(imageVersion)

start:
	cd hack; okteto up --file okteto.yml

stop:
	cd hack; okteto down --file okteto.yml