.PHONY: run dev build vet tidy up down

run:
	go run ./cmd/server

# adk's launcher over the same agents: make dev web | make dev cli | make dev api
dev:
	go run ./cmd/dev $(filter-out $@,$(MAKECMDGOALS))

%:
	@:

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
