.PHONY: run dev build vet lint test tidy indices backfill

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

# Creates the OpenSearch indices from deploy/opensearch, or adds new fields to
# existing ones; the service never changes a mapping (docs/ARCHITECTURE.md).
# Each index is named with the server's prefix (OPENSEARCH_INDEX_PREFIX); the
# chunks' and memories' with the names models.yaml gives them, when it does
# (embedding.index, memory.index): make indices CHUNK_INDEX=… MEMORY_INDEX=….
# These targets authenticate as OpenSearch's internal admin user (the defaults
# are the local stack's, ~/code/agentlite-infra); the server never does
# (docs/AUTH.md).
OPENSEARCH_URL ?= http://localhost:9200
OPENSEARCH_INDEX_PREFIX ?= user_ai_
OPENSEARCH_USERNAME ?= admin
OPENSEARCH_PASSWORD ?= Agentlite-Dev-Admin-2026
CHUNK_INDEX ?= userfile_chunks
MEMORY_INDEX ?= memories
CURL = curl -sSf -H 'Content-Type: application/json' -u '$(OPENSEARCH_USERNAME):$(OPENSEARCH_PASSWORD)'
indices:
	@for f in deploy/opensearch/*.json; do n=$$(basename $$f .json); \
	  case $$n in userfile_chunks) n=$(CHUNK_INDEX);; memories) n=$(MEMORY_INDEX);; esac; i=$(OPENSEARCH_INDEX_PREFIX)$$n; \
	  if $(CURL) -o /dev/null $(OPENSEARCH_URL)/$$i 2>/dev/null; then \
	    jq .mappings $$f | $(CURL) -XPUT $(OPENSEARCH_URL)/$$i/_mapping -d @-; \
	  else $(CURL) -XPUT $(OPENSEARCH_URL)/$$i -d @$$f; fi && echo " $$i"; done

# Labels every document that has no control set yet (docs/AUTH.md): in every
# index named with the prefix, sets controlset_id (BACKFILL_CONTROLSET) and,
# where missing, user_created_by and user_updated_by (BACKFILL_USER). It first
# adds the three fields to each index's mapping, so indices that make indices
# does not manage take them too. Documents without a controlset_id are
# invisible to every user once DLS is on. Safe to run again: it only touches
# unlabelled ones.
BACKFILL_CONTROLSET ?= C1000
BACKFILL_USER ?= alice
BACKFILL_MAPPING = {"properties": {"controlset_id": {"type": "keyword"}, "user_created_by": {"type": "keyword"}, "user_updated_by": {"type": "keyword"}}}
BACKFILL_QUERY = {"query": {"bool": {"must_not": {"exists": {"field": "controlset_id"}}}}, "script": {"lang": "painless", "params": {"cs": "$(BACKFILL_CONTROLSET)", "user": "$(BACKFILL_USER)"}, "source": "ctx._source.controlset_id = params.cs; if (ctx._source.user_created_by == null) { ctx._source.user_created_by = params.user } if (ctx._source.user_updated_by == null) { ctx._source.user_updated_by = params.user }"}}
backfill:
	@for i in $$($(CURL) '$(OPENSEARCH_URL)/_cat/indices/$(OPENSEARCH_INDEX_PREFIX)*?h=index&s=index'); do \
	  $(CURL) -o /dev/null -XPUT $(OPENSEARCH_URL)/$$i/_mapping -d '$(BACKFILL_MAPPING)' && \
	  r=$$($(CURL) -XPOST "$(OPENSEARCH_URL)/$$i/_update_by_query?conflicts=proceed&refresh=true" -d '$(BACKFILL_QUERY)') && \
	  echo "$$r" | jq -r --arg i $$i '"\($$i): \(.updated) of \(.total) labelled, \(.failures | length) failures"' || exit 1; done
