// Package config is the backend's configuration: what a deployment states and
// what the code refuses to guess.
//
// Everything here comes from the environment, because the chart's values become
// environment variables and a second configuration format would be a second
// place for an operator to look. Two rules shape what is in it: an operational
// parameter the clusters must agree on lives here and is handed out at
// registration, and a credential is a reference to a secret rather than a
// literal wherever the platform can manage one.
package config

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	clusterv1 "github.com/automagicops/haliphron/api/cluster/v1"
	runv1 "github.com/automagicops/haliphron/api/run/v1"
	"github.com/automagicops/haliphron/backend/app"
	"github.com/automagicops/haliphron/backend/artifacts"
	"github.com/automagicops/haliphron/backend/run"
	"github.com/automagicops/haliphron/backend/store"
)

// Config is the whole of it.
type Config struct {
	Mode Mode

	PublicAddr  string
	MCPAddr     string
	ClusterAddr string
	MetricsAddr string

	Database store.Config
	Storage  artifacts.S3Config

	// KEKID and KEK wrap the per-secret data keys. The key never enters the
	// database, which is the entire property: a dump is not a credential leak.
	KEKID string
	KEK   []byte

	Timings  clusterv1.Timings
	Versions clusterv1.VersionRange
	Defaults run.Defaults
	Limits   app.Limits

	SweepInterval time.Duration
	ReapInterval  time.Duration

	LogLevel  string
	LogFormat string

	// Migrate applies the schema at startup. On by default, because every
	// replica calling it is safe — the advisory lock makes it so — and an
	// operator who wants to apply migrations by hand can say so.
	Migrate bool
}

// Mode selects which listeners run.
//
// The default runs all of them in one process. The split exists for the
// deployment where MCP has to be exposed outward while REST stays inside the
// perimeter; it is a configuration decision rather than an architectural one,
// and the same binary serves either way.
type Mode string

const (
	ModeAll     Mode = "all"
	ModeAPI     Mode = "api"
	ModeMCP     Mode = "mcp"
	ModeCluster Mode = "cluster"
)

// Serves reports whether this mode runs a listener.
func (m Mode) Serves(what Mode) bool { return m == ModeAll || m == what }

// Load reads the environment and fills in what a deployment did not state.
func Load() (Config, error) {
	cfg := Config{
		Mode:        Mode(env("HALIPHRON_MODE", string(ModeAll))),
		PublicAddr:  env("HALIPHRON_PUBLIC_ADDR", ":8080"),
		MCPAddr:     env("HALIPHRON_MCP_ADDR", ":8081"),
		ClusterAddr: env("HALIPHRON_CLUSTER_ADDR", ":8082"),
		MetricsAddr: env("HALIPHRON_METRICS_ADDR", ":9090"),

		Database: store.Config{
			DSN:             os.Getenv("HALIPHRON_DSN"),
			MaxOpenConns:    intEnv("HALIPHRON_DB_MAX_CONNS", 16),
			MaxIdleConns:    intEnv("HALIPHRON_DB_IDLE_CONNS", 4),
			ConnMaxLifetime: durationEnv("HALIPHRON_DB_CONN_LIFETIME", time.Hour),
		},

		Storage: artifacts.S3Config{
			Bucket:       env("HALIPHRON_S3_BUCKET", "haliphron"),
			Region:       env("HALIPHRON_S3_REGION", "us-east-1"),
			Endpoint:     os.Getenv("HALIPHRON_S3_ENDPOINT"),
			AccessKey:    os.Getenv("HALIPHRON_S3_ACCESS_KEY"),
			SecretKey:    os.Getenv("HALIPHRON_S3_SECRET_KEY"),
			SessionToken: os.Getenv("HALIPHRON_S3_SESSION_TOKEN"),
			PathStyle:    boolEnv("HALIPHRON_S3_PATH_STYLE", false),
		},

		KEKID: env("HALIPHRON_KEK_ID", "default"),

		Timings: clusterv1.Timings{
			HeartbeatIntervalSeconds: int32Env("HALIPHRON_HEARTBEAT_INTERVAL_SECONDS",
				clusterv1.DefaultTimings().HeartbeatIntervalSeconds),
			StaleAfterSeconds: int32Env("HALIPHRON_STALE_AFTER_SECONDS",
				clusterv1.DefaultTimings().StaleAfterSeconds),
			LeaseTTLSeconds: int32Env("HALIPHRON_LEASE_TTL_SECONDS",
				clusterv1.DefaultTimings().LeaseTTLSeconds),
			AckTimeoutSeconds: int32Env("HALIPHRON_ACK_TIMEOUT_SECONDS",
				clusterv1.DefaultTimings().AckTimeoutSeconds),
			MaxWaitSeconds: int32Env("HALIPHRON_MAX_WAIT_SECONDS",
				clusterv1.DefaultTimings().MaxWaitSeconds),
			MaxLeasesPerPoll: int32Env("HALIPHRON_MAX_LEASES_PER_POLL",
				clusterv1.DefaultTimings().MaxLeasesPerPoll),
			ArtifactTTLMultiplier: floatEnv("HALIPHRON_ARTIFACT_TTL_MULTIPLIER",
				clusterv1.DefaultTimings().ArtifactTTLMultiplier),
		},
		Versions: clusterv1.VersionRange{
			Min: env("HALIPHRON_MIN_CONTROLLER_VERSION", "0.1.0"),
			Max: env("HALIPHRON_MAX_CONTROLLER_VERSION", "99.0.0"),
		},

		Defaults: run.Defaults{
			Image:                   os.Getenv("HALIPHRON_AGENT_IMAGE"),
			ImagePullPolicy:         env("HALIPHRON_AGENT_IMAGE_PULL_POLICY", "IfNotPresent"),
			Model:                   env("HALIPHRON_DEFAULT_MODEL", "anthropic/claude-opus-5"),
			Agent:                   runv1.AgentType(env("HALIPHRON_DEFAULT_AGENT", string(runv1.AgentClaudeCode))),
			TimeoutSeconds:          int32Env("HALIPHRON_DEFAULT_TIMEOUT_SECONDS", 3600),
			TTLSeconds:              int32Env("HALIPHRON_RUN_TTL_SECONDS", 86400),
			MaxInfraRetries:         int32Env("HALIPHRON_MAX_INFRA_RETRIES", 3),
			OTLPEndpoint:            os.Getenv("HALIPHRON_OTLP_ENDPOINT"),
			LogChunkIntervalSeconds: int32Env("HALIPHRON_LOG_CHUNK_INTERVAL_SECONDS", 5),
			MaxPromptBytes:          intEnv("HALIPHRON_MAX_PROMPT_BYTES", run.MaxPromptBytes),
			MaxDepth:                int16(intEnv("HALIPHRON_MAX_RUN_DEPTH", run.MaxDepth)),
		},

		Limits: app.Limits{
			LLMAPIKeySecret:       env("HALIPHRON_LLM_SECRET", "llm-api-key"),
			GitTokenSecret:        env("HALIPHRON_GIT_SECRET", "git-token"),
			MCPEndpoint:           os.Getenv("HALIPHRON_MCP_ENDPOINT"),
			RunTokenTTLMultiplier: floatEnv("HALIPHRON_RUN_TOKEN_TTL_MULTIPLIER", 2),
			IdempotencyTTL:        durationEnv("HALIPHRON_IDEMPOTENCY_TTL", 24*time.Hour),
		},

		SweepInterval: durationEnv("HALIPHRON_SWEEP_INTERVAL", 5*time.Second),
		ReapInterval:  durationEnv("HALIPHRON_REAP_INTERVAL", time.Hour),

		LogLevel:  env("HALIPHRON_LOG_LEVEL", "info"),
		LogFormat: env("HALIPHRON_LOG_FORMAT", "json"),
		Migrate:   boolEnv("HALIPHRON_MIGRATE", true),
	}

	if raw := os.Getenv("HALIPHRON_TOOL_DENY"); raw != "" {
		// The policy ceiling. A deny here cannot be lifted by a role, which is
		// what makes it a ceiling rather than a default.
		cfg.Defaults.ToolPolicyCeiling = &runv1.ToolPolicy{Deny: splitList(raw)}
	}
	if raw := os.Getenv("HALIPHRON_TOOL_ALLOW"); raw != "" {
		if cfg.Defaults.ToolPolicyCeiling == nil {
			cfg.Defaults.ToolPolicyCeiling = &runv1.ToolPolicy{}
		}
		cfg.Defaults.ToolPolicyCeiling.Allow = splitList(raw)
	}

	kek, err := loadKEK()
	if err != nil {
		return Config{}, err
	}
	cfg.KEK = kek

	return cfg, cfg.validate()
}

// loadKEK reads the key encryption key from a variable or a file.
//
// A file is the better of the two and is checked first: an environment
// variable is visible in `kubectl describe pod` and in every crash dump, while
// a mounted file is readable by the process and nothing else.
func loadKEK() ([]byte, error) {
	if path := os.Getenv("HALIPHRON_KEK_FILE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		return decodeKEK(strings.TrimSpace(string(raw)))
	}
	if raw := os.Getenv("HALIPHRON_KEK"); raw != "" {
		return decodeKEK(raw)
	}
	// No key: managed secrets are unavailable and referenced ones still work.
	// Refusing to start would make the key mandatory for an installation whose
	// secrets all live in Vault.
	return nil, nil
}

// decodeKEK accepts the two shapes a key arrives in, and derives one from a
// passphrase rather than refusing it: an operator who supplies 20 characters
// gets a usable key instead of a start-up failure they will work around by
// pasting 32.
func decodeKEK(raw string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(raw) < 16 {
		return nil, fmt.Errorf("config: the key encryption key is too short to derive from")
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
}

func (c Config) validate() error {
	switch c.Mode {
	case ModeAll, ModeAPI, ModeMCP, ModeCluster:
	default:
		return fmt.Errorf("config: unknown mode %q", c.Mode)
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("config: HALIPHRON_DSN is required")
	}
	if c.Defaults.Image == "" {
		// There is no sensible default: the image is pinned by digest, and a
		// tag chosen here would silently decide what every run executes.
		return fmt.Errorf("config: HALIPHRON_AGENT_IMAGE is required")
	}
	if c.Storage.AccessKey == "" || c.Storage.SecretKey == "" {
		return fmt.Errorf("config: object storage credentials are required")
	}
	if c.Timings.MaxWaitSeconds > 30 {
		// The ingress's proxy_read_timeout must be greater than this, and 30
		// is what the contract states. A longer wait turns expired polls into
		// dropped connections, which reads as network instability.
		return fmt.Errorf("config: max wait is %ds, the contract's ceiling is 30s",
			c.Timings.MaxWaitSeconds)
	}
	return nil
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func intEnv(name string, fallback int) int {
	v, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return v
}

func int32Env(name string, fallback int32) int32 { return int32(intEnv(name, int(fallback))) }

func floatEnv(name string, fallback float32) float32 {
	v, err := strconv.ParseFloat(os.Getenv(name), 32)
	if err != nil {
		return fallback
	}
	return float32(v)
}

func boolEnv(name string, fallback bool) bool {
	v, err := strconv.ParseBool(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return v
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	v, err := time.ParseDuration(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return v
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}
