PROG_NAME := "jqurl"
IMAGE_NAME := "pschou/jqurl"
VERSION = 0.1.$(shell date -u +%Y%m%d.%H%M)
FLAGS := "-s -w -X main.version=${VERSION}"


build:
	CGO_ENABLED=0 go build -ldflags=${FLAGS} -o ${PROG_NAME} main.go
	upx --lzma ${PROG_NAME}
	GOOS=windows GOARCH=386 CGO_ENABLED=0 go build -ldflags=${FLAGS} -o ${PROG_NAME}.exe main.go
	upx --lzma ${PROG_NAME}.exe
	GOOS=solaris GOARCH=amd64 CGO_ENABLED=0 go build -ldflags=${FLAGS} -o ${PROG_NAME}_solaris64 main.go
	upx --lzma ${PROG_NAME}_solaris64
	#GOOS=solaris GOARCH=sparc64 CGO_ENABLED=0 go build -ldflags=${FLAGS} -o ${PROG_NAME}_sparc64 main.go

jq:
	CGO_ENABLED=0 go build -ldflags=${FLAGS} -o ${PROG_NAME} main.go

docker:
	docker build -f Dockerfile --tag ${IMAGE_NAME}:${VERSION} .
	docker push ${IMAGE_NAME}:${VERSION}; \
	docker save -o pschou_${PROG_NAME}.tar ${IMAGE_NAME}:${VERSION}
