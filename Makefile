build:
	docker build . -t golang-fuse:latest

run:
	docker run --privileged -it golang-fuse:latest