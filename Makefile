imageVersion := latest

build:
	okteto build --build-arg BUILD_ENV=okteto -f ./Dockerfile -t localhost:5001/beam-clip:$(imageVersion)

start:
	cd hack; okteto up --file okteto.yml

stop:
	cd hack; okteto down --file okteto.yml