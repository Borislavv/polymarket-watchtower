package log

import (
	"io"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

const (
	envLocal       = "local"
	envDevelopment = "development"
	envDev         = "dev"
)

type Config struct {
	Env        string
	Level      string
	Service    string
	Version    string
	Pretty     bool
	WithCaller bool
}

func New() *zerolog.Logger {
	return NewWithConfig(Config{
		Env:        getenv("APP_ENV", "production"),
		Level:      getenv("LOG_LEVEL", "info"),
		Service:    getenv("SERVICE_NAME", ""),
		Version:    getenv("SERVICE_VERSION", ""),
		Pretty:     isPrettyEnv(getenv("APP_ENV", "production")),
		WithCaller: getenv("LOG_CALLER", "true") == "true",
	})
}

func NewWithConfig(cfg Config) *zerolog.Logger {
	configureGlobals()

	level, err := zerolog.ParseLevel(strings.ToLower(cfg.Level))
	if err != nil {
		level = zerolog.InfoLevel
	}

	zerolog.SetGlobalLevel(level)

	writer := buildWriter(cfg)

	ctx := zerolog.New(writer).
		Level(level).
		With().
		Timestamp()

	if cfg.WithCaller {
		ctx = ctx.Caller()
	}

	if cfg.Service != "" {
		ctx = ctx.Str("service", cfg.Service)
	}

	if cfg.Version != "" {
		ctx = ctx.Str("version", cfg.Version)
	}

	if cfg.Env != "" {
		ctx = ctx.Str("env", cfg.Env)
	}

	logger := ctx.Logger()

	return &logger
}

func buildWriter(cfg Config) io.Writer {
	if !cfg.Pretty {
		return os.Stderr
	}

	return zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: time.RFC3339,
		NoColor:    os.Getenv("NO_COLOR") != "",
	}
}

func configureGlobals() {
	zerolog.TimeFieldFormat = time.RFC3339Nano

	zerolog.TimestampFieldName = "timestamp"
	zerolog.LevelFieldName = "level"
	zerolog.MessageFieldName = "message"
	zerolog.ErrorFieldName = "error"
	zerolog.CallerFieldName = "caller"

	zerolog.DurationFieldUnit = time.Millisecond
	zerolog.DurationFieldInteger = false
}

func isPrettyEnv(env string) bool {
	switch strings.ToLower(env) {
	case envLocal, envDevelopment, envDev:
		return true
	default:
		return false
	}
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	return value
}
