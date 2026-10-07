.PHONY: run build vet tidy up down

run:
	go run ./cmd/server

build:
	go build -o bin/server ./cmd/server

vet:
	go vet ./...

tidy:
	go mod tidy

up:
	docker compose up -d

down:
	docker compose down
