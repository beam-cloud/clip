imageVersion := latest

.PHONY: e2e

build:
	okteto build --build-arg BUILD_ENV=okteto -f ./Dockerfile -t localhost:5001/beam-clip:$(imageVersion)

start:
	cd hack; okteto up --file okteto.yml

stop:
	cd hack; okteto down --file okteto.yml

e2e:
	go build -o ./bin/e2e ./e2e/main.go

clipctl:
	go build -o ./bin/clipctl ./cmd/clipctl/main.go

install-clipctl: clipctl
	sudo cp ./bin/clipctl /usr/local/bin/clipctl

