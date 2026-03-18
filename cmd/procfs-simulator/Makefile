.PHONY: build build-amd64 build-arm64 install docker-build
build:
	CGO_ENABLED=0 go build -o procfs-simulator .

build-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o procfs-simulator .

build-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o procfs-simulator .

# 安装到 /usr/local/bin（需先 make build 或 ./build.sh）
install:
	install -m 755 procfs-simulator /usr/local/bin/
	install -m 755 hook-fuse.sh /usr/local/bin/
	install -m 755 runc-wrapper-fuse.sh /usr/local/bin/hide-overlayfs-runc

docker-build:
	docker build -t procfs-simulator:latest .
