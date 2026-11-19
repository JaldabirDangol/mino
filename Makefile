# Common tasks. Tests need a PostgreSQL database; `make db` starts one in Docker.
TEST_DB ?= postgres://mino:mino@127.0.0.1:5440/mino?sslmode=disable
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build run dev test assets docker db

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/mino ./cmd/mino

run: build
	./bin/mino

# Reads templates and static files from disk, so edits show on reload.
dev:
	MINO_DEV=true go run ./cmd/mino

test:
	MINO_TEST_DATABASE_URL="$(TEST_DB)" go test -race -count=1 ./...

# Rebuild admin CSS, the rich-text editor bundle and vendored JS/fonts (needs Node).
assets:
	cd web && npm ci && npm run build

docker:
	docker build --build-arg VERSION=$(VERSION) -t mino:$(VERSION) -t mino:latest .

# Development/test database on port 5440.
db:
	docker run -d --name mino-dev-db -e POSTGRES_USER=mino -e POSTGRES_PASSWORD=mino -e POSTGRES_DB=mino \
		-p 127.0.0.1:5440:5432 postgres:17
