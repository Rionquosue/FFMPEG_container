.PHONY: test build clean
test:
	go test -race -count=1 ./...
	go vet ./...
	cc -O2 -Wall -Wextra -Werror -o /tmp/media-sandbox-check sandbox/media-sandbox.c
	/tmp/media-sandbox-check --check-egress
build:
	mkdir -p bin
	CGO_ENABLED=0 go build -buildvcs=false -trimpath -o bin/ffmpeg-api ./cmd/server
	cc -O2 -static -Wall -Wextra -Werror -o bin/media-sandbox sandbox/media-sandbox.c
clean:
	rm -rf bin
