module agentlite

go 1.27.1

// archive/zip refuses a zip with a path that leaves its root (../, /, \):
// an uploaded skill (internal/skills).
godebug zipinsecurepath=0

require (
	dario.cat/mergo v1.0.2
	github.com/adrg/frontmatter v0.2.0
	github.com/ag-ui-protocol/ag-ui/sdks/community/go v0.0.0-20261007133343-903a9ab9a154
	github.com/coder/websocket v1.8.15
	github.com/expr-lang/expr v1.17.8
	github.com/gabriel-vasile/mimetype v1.4.16
	github.com/goccy/go-yaml v1.19.2
	github.com/google/go-cmp v0.7.0
	github.com/google/uuid v1.6.0
	github.com/joho/godotenv v1.5.1
	github.com/labstack/echo/v5 v5.4.0
	github.com/minio/minio-go/v7 v7.3.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
	github.com/openai/openai-go/v3 v3.71.1
	github.com/opensearch-project/opensearch-go/v5 v5.0.0
	github.com/rs/zerolog v1.35.1
	github.com/samber/slog-zerolog/v2 v2.7.3
	github.com/sethvargo/go-envconfig v1.4.3
	github.com/tmc/langchaingo v0.1.15
	github.com/valkey-io/valkey-go v1.0.78
	github.com/youmark/pkcs8 v0.0.0-20240726163527-a2c0da244d78
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/trace v1.47.0
	golang.org/x/net v0.59.0
	google.golang.org/adk/v2 v2.6.1-0.20261016232144-74faa9ab6bf5
	google.golang.org/genai v1.74.0
)

require (
	cloud.google.com/go v0.123.0 // indirect
	cloud.google.com/go/auth v0.24.0 // indirect
	cloud.google.com/go/auth/oauth2adapt v0.3.0 // indirect
	cloud.google.com/go/compute/metadata v0.10.0 // indirect
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/a2aproject/a2a-go v0.3.15 // indirect
	github.com/a2aproject/a2a-go/v2 v2.6.0 // indirect
	github.com/awalterschulze/gographviz v2.0.3+incompatible // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/dlclark/regexp2 v1.12.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/felixge/httpsnoop v1.1.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/s2a-go v0.1.10 // indirect
	github.com/google/safehtml v0.1.0 // indirect
	github.com/googleapis/enterprise-certificate-proxy v0.3.22 // indirect
	github.com/googleapis/gax-go/v2 v2.26.2 // indirect
	github.com/gorilla/mux v1.8.1 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.30.0 // indirect
	github.com/hashicorp/golang-lru/v2 v2.0.7 // indirect
	github.com/klauspost/compress v1.19.2 // indirect
	github.com/klauspost/cpuid/v2 v2.4.0 // indirect
	github.com/klauspost/crc32 v1.3.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/minio/crc64nvme v1.1.1 // indirect
	github.com/minio/md5-simd v1.1.2 // indirect
	github.com/mitchellh/mapstructure v1.5.0 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/pkoukk/tiktoken-go v0.1.6 // indirect
	github.com/rs/dnscache v0.0.0-20230804202142-fc85eb664529 // indirect
	github.com/rs/xid v1.6.0 // indirect
	github.com/samber/lo v1.47.0 // indirect
	github.com/samber/slog-common v0.18.1 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.2.0 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	github.com/zeebo/xxh3 v1.1.0 // indirect
	gitlab.com/golang-commonmark/html v0.0.0-20191124015941-a22733972181 // indirect
	gitlab.com/golang-commonmark/linkify v0.0.0-20191026162114-a0c2df6c8f82 // indirect
	gitlab.com/golang-commonmark/markdown v0.0.0-20211110145824-bf3e522c626a // indirect
	gitlab.com/golang-commonmark/mdurl v0.0.0-20191124015652-932350d1cb84 // indirect
	gitlab.com/golang-commonmark/puny v0.0.0-20191124015043-9f83538fa04f // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/detectors/gcp v1.47.0 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.70.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp v0.23.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/log v1.47.0 // indirect
	go.opentelemetry.io/proto/otlp v1.11.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/mod v0.41.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	google.golang.org/api v0.300.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/grpc v1.84.0 // indirect
	google.golang.org/protobuf v1.36.12 // indirect
	gopkg.in/ini.v1 v1.67.3 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	rsc.io/omap v1.2.0 // indirect
	rsc.io/ordered v1.1.1 // indirect
)
