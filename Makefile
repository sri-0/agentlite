.PHONY: run dev build vet lint test tidy up down indices

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

lint: vet
	go run honnef.co/go/tools/cmd/staticcheck@v0.8.2 ./...

test:
	go test ./...

tidy:
	go mod tidy

up:
	docker compose up -d

down:
	docker compose down

# Creates the OpenSearch indices from deploy/opensearch, or adds new fields to
# existing ones; the service never changes a mapping (docs/ARCHITECTURE.md).
# Each index is named with the server's prefix (OPENSEARCH_INDEX_PREFIX); the
# chunks' and memories' with the names models.yaml gives them, when it does
# (embedding.index, memory.index): make indices CHUNK_INDEX=… MEMORY_INDEX=….
OPENSEARCH_URL ?= http://localhost:9200
OPENSEARCH_INDEX_PREFIX ?= user_ai_
CHUNK_INDEX ?= userfile_chunks
MEMORY_INDEX ?= memories
CURL = curl -sSf -H 'Content-Type: application/json' $(if $(OPENSEARCH_USERNAME),-u '$(OPENSEARCH_USERNAME):$(OPENSEARCH_PASSWORD)')
indices:
	@for f in deploy/opensearch/*.json; do n=$$(basename $$f .json); \
	  case $$n in userfile_chunks) n=$(CHUNK_INDEX);; memories) n=$(MEMORY_INDEX);; esac; i=$(OPENSEARCH_INDEX_PREFIX)$$n; \
	  if $(CURL) -o /dev/null $(OPENSEARCH_URL)/$$i 2>/dev/null; then \
	    jq .mappings $$f | $(CURL) -XPUT $(OPENSEARCH_URL)/$$i/_mapping -d @-; \
	  else $(CURL) -XPUT $(OPENSEARCH_URL)/$$i -d @$$f; fi && echo " $$i"; done
