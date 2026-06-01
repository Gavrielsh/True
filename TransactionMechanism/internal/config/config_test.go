package config

import (
	"strings"
	"testing"
	"time"
)

// Note: these tests use t.Setenv, which is incompatible with t.Parallel().

func TestParseOperatorSecrets(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "json_form",
			raw:  `{"PRAGMATIC":"sec1","HACKSAW":"sec2"}`,
			want: map[string]string{"PRAGMATIC": "sec1", "HACKSAW": "sec2"},
		},
		{
			name: "csv_form",
			raw:  `PRAGMATIC:sec1,HACKSAW:sec2`,
			want: map[string]string{"PRAGMATIC": "sec1", "HACKSAW": "sec2"},
		},
		{
			name: "csv_secret_with_colon",
			raw:  `OP:a:b:c`,
			want: map[string]string{"OP": "a:b:c"},
		},
		{
			name: "csv_with_spaces_trimmed",
			raw:  ` OP1 : sec1 , OP2 : sec2 `,
			want: map[string]string{"OP1": "sec1", "OP2": "sec2"},
		},
		{
			name: "csv_trailing_comma_ok",
			raw:  `OP1:sec1,`,
			want: map[string]string{"OP1": "sec1"},
		},
		{"empty", "", nil, true},
		{"whitespace_only", "   ", nil, true},
		{"json_invalid", `{"OP":}`, nil, true},
		{"json_empty_object", `{}`, nil, true},
		{"json_empty_secret", `{"OP":""}`, nil, true},
		{"csv_no_colon", `OP1sec1`, nil, true},
		{"csv_empty_code", `:sec1`, nil, true},
		{"csv_empty_secret", `OP1:`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOperatorSecrets(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len: got %d (%v) want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("key %q: got %q want %q", k, got[k], v)
				}
			}
		})
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("POSTGRES_URL", "postgres://u:p@localhost:5432/db")
	t.Setenv("REDIS_URL", "redis://localhost:6379/0")
	t.Setenv("OPERATOR_SECRETS", `{"OP1":"secret"}`)
}

func TestLoad_Defaults(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port default: got %s want 8080", cfg.Port)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel default: got %s", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout default: got %s", cfg.ShutdownTimeout)
	}
	if cfg.MaxDBConns <= 0 {
		t.Errorf("MaxDBConns must default positive, got %d", cfg.MaxDBConns)
	}
	if cfg.OperatorSecrets["OP1"] != "secret" {
		t.Errorf("secret not loaded: %v", cfg.OperatorSecrets)
	}
}

func TestLoad_Overrides(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("PORT", "9000")
	t.Setenv("LOG_LEVEL", "debug")
	t.Setenv("SHUTDOWN_TIMEOUT", "5s")
	t.Setenv("MAX_DB_CONNS", "42")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != "9000" {
		t.Errorf("Port: got %s want 9000", cfg.Port)
	}
	if cfg.ShutdownTimeout != 5*time.Second {
		t.Errorf("ShutdownTimeout: got %s want 5s", cfg.ShutdownTimeout)
	}
	if cfg.MaxDBConns != 42 {
		t.Errorf("MaxDBConns: got %d want 42", cfg.MaxDBConns)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	cases := []string{"POSTGRES_URL", "REDIS_URL", "OPERATOR_SECRETS"}
	for _, missing := range cases {
		t.Run("missing_"+missing, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(missing, "") // unset the one under test
			_, err := Load()
			if err == nil {
				t.Fatalf("expected error when %s is missing", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error %q should name %s", err, missing)
			}
		})
	}
}

func TestLoad_MalformedValues(t *testing.T) {
	cases := []struct {
		name string
		key  string
		val  string
	}{
		{"bad_duration", "SHUTDOWN_TIMEOUT", "not-a-duration"},
		{"zero_duration", "HTTP_READ_TIMEOUT", "0s"},
		{"bad_int", "MAX_DB_CONNS", "abc"},
		{"negative_int", "MAX_DB_CONNS", "-3"},
		{"bad_secrets", "OPERATOR_SECRETS", "no-colon-here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s=%q", tc.key, tc.val)
			}
		})
	}
}

func TestConfig_LogValueHidesSecrets(t *testing.T) {
	cfg := Config{
		Port:            "8080",
		PostgresURL:     "postgres://user:SUPERSECRET@host/db",
		RedisURL:        "redis://:REDISPASS@host:6379",
		OperatorSecrets: map[string]string{"OP1": "HMACSECRET"},
		MaxDBConns:      9,
		ShutdownTimeout: 30 * time.Second,
	}
	rendered := cfg.LogValue().String()
	for _, leak := range []string{"SUPERSECRET", "REDISPASS", "HMACSECRET", "postgres://", "redis://"} {
		if strings.Contains(rendered, leak) {
			t.Errorf("LogValue leaked %q: %s", leak, rendered)
		}
	}
	// It should still expose the safe operator count.
	if !strings.Contains(rendered, "operator_count") {
		t.Errorf("LogValue missing operator_count: %s", rendered)
	}
}
