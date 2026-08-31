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
	t.Setenv("MAX_PROVIDER_WIN", "50000.0000")
	// Jurisdiction enforcement is now on by default, so a config fixture must
	// either supply a GeoIP database path or take the explicit opt-out. The
	// tests here exercise config parsing, not geo behaviour, so they opt out.
	t.Setenv("GEOFENCE_MODE", "disabled")
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
	if cfg.DB.MaxConns <= 0 {
		t.Errorf("DB.MaxConns must default positive, got %d", cfg.DB.MaxConns)
	}
	if cfg.DB.MinConns <= 0 || cfg.DB.MinConns > cfg.DB.MaxConns {
		t.Errorf("DB.MinConns must default into (0, MaxConns]: got %d (max %d)", cfg.DB.MinConns, cfg.DB.MaxConns)
	}
	if cfg.DB.MaxConnIdleTime != 30*time.Minute {
		t.Errorf("DB.MaxConnIdleTime default: got %s want 30m", cfg.DB.MaxConnIdleTime)
	}
	if cfg.DB.MaxConnLifetime != time.Hour {
		t.Errorf("DB.MaxConnLifetime default: got %s want 1h", cfg.DB.MaxConnLifetime)
	}
	if cfg.DB.HealthCheckPeriod != time.Minute {
		t.Errorf("DB.HealthCheckPeriod default: got %s want 1m", cfg.DB.HealthCheckPeriod)
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
	t.Setenv("DB_MAX_CONNS", "42")
	t.Setenv("DB_MIN_CONNS", "8")
	t.Setenv("DB_MAX_CONN_IDLE_TIME", "90s")
	t.Setenv("DB_MAX_CONN_LIFETIME", "2h")
	t.Setenv("DB_HEALTH_CHECK_PERIOD", "30s")

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
	if cfg.DB.MaxConns != 42 {
		t.Errorf("DB.MaxConns: got %d want 42", cfg.DB.MaxConns)
	}
	if cfg.DB.MinConns != 8 {
		t.Errorf("DB.MinConns: got %d want 8", cfg.DB.MinConns)
	}
	if cfg.DB.MaxConnIdleTime != 90*time.Second {
		t.Errorf("DB.MaxConnIdleTime: got %s want 90s", cfg.DB.MaxConnIdleTime)
	}
	if cfg.DB.MaxConnLifetime != 2*time.Hour {
		t.Errorf("DB.MaxConnLifetime: got %s want 2h", cfg.DB.MaxConnLifetime)
	}
	if cfg.DB.HealthCheckPeriod != 30*time.Second {
		t.Errorf("DB.HealthCheckPeriod: got %s want 30s", cfg.DB.HealthCheckPeriod)
	}
}

// The legacy MAX_DB_CONNS name remains a fallback for DB_MAX_CONNS so existing
// deploys keep working; DB_MAX_CONNS wins when both are set.
func TestLoad_LegacyMaxDBConnsAlias(t *testing.T) {
	t.Run("legacy_only", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MAX_DB_CONNS", "17")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DB.MaxConns != 17 {
			t.Errorf("DB.MaxConns from legacy alias: got %d want 17", cfg.DB.MaxConns)
		}
	})
	t.Run("new_wins_over_legacy", func(t *testing.T) {
		setRequiredEnv(t)
		t.Setenv("MAX_DB_CONNS", "17")
		t.Setenv("DB_MAX_CONNS", "33")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.DB.MaxConns != 33 {
			t.Errorf("DB_MAX_CONNS must win: got %d want 33", cfg.DB.MaxConns)
		}
	})
}

// MinConns may legitimately be 0 (no warm floor); the loader must accept it.
func TestLoad_MinConnsZeroAllowed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DB_MIN_CONNS", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.MinConns != 0 {
		t.Errorf("DB.MinConns: got %d want 0", cfg.DB.MinConns)
	}
}

func TestLoad_MissingRequired(t *testing.T) {
	cases := []string{"POSTGRES_URL", "REDIS_URL", "OPERATOR_SECRETS", "MAX_PROVIDER_WIN"}
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
		{"bad_int", "DB_MAX_CONNS", "abc"},
		{"negative_int", "DB_MAX_CONNS", "-3"},
		{"zero_max_conns", "DB_MAX_CONNS", "0"},
		{"negative_min_conns", "DB_MIN_CONNS", "-1"},
		{"bad_idle_duration", "DB_MAX_CONN_IDLE_TIME", "nope"},
		{"bad_lifetime_duration", "DB_MAX_CONN_LIFETIME", "5"},
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

// MinConns must not exceed MaxConns — a contradictory pool sizing must fail
// fast at boot, never silently clamp.
func TestLoad_MinConnsExceedsMaxConns(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DB_MAX_CONNS", "4")
	t.Setenv("DB_MIN_CONNS", "9")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when DB_MIN_CONNS > DB_MAX_CONNS")
	}
	if !strings.Contains(err.Error(), "DB_MIN_CONNS") {
		t.Errorf("error should name DB_MIN_CONNS: %v", err)
	}
}

func TestConfig_LogValueHidesSecrets(t *testing.T) {
	cfg := Config{
		Port:            "8080",
		PostgresURL:     "postgres://user:SUPERSECRET@host/db",
		RedisURL:        "redis://:REDISPASS@host:6379",
		OperatorSecrets: map[string]string{"OP1": "HMACSECRET"},
		DB:              DBConfig{MaxConns: 9, MinConns: 2, MaxConnIdleTime: 30 * time.Minute, MaxConnLifetime: time.Hour},
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

// ----------------------------------------------------------------------------
// Jurisdiction enforcement must be on by default (audit fix)
// ----------------------------------------------------------------------------

// The previous behaviour — an unset GEOIP_DB_PATH silently disabling the
// fence — is exactly how the blocklist ended up looking configured while
// enforcing nothing. Booting without a database now requires an explicit,
// greppable opt-out.
func TestLoad_GeofenceRequiresDBUnlessExplicitlyDisabled(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("GEOFENCE_MODE", "") // clear the fixture's opt-out
	t.Setenv("GEOIP_DB_PATH", "")

	if _, err := Load(); err == nil {
		t.Fatal("an unset GEOIP_DB_PATH must fail the boot, not silently disable enforcement")
	}
}

func TestLoad_GeofenceDisabledOptOut(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("GEOFENCE_MODE", "disabled")
	t.Setenv("GEOIP_DB_PATH", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("explicit opt-out should boot: %v", err)
	}
	if !cfg.GeofenceDisabled {
		t.Error("GeofenceDisabled should be true")
	}
}

func TestLoad_GeofenceEnabledWithDBPath(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("GEOFENCE_MODE", "")
	t.Setenv("GEOIP_DB_PATH", "/opt/geoip/GeoLite2-City.mmdb")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("a configured db path should boot: %v", err)
	}
	if cfg.GeofenceDisabled {
		t.Error("GeofenceDisabled should be false when a db path is set")
	}
	if cfg.GeoIPDBPath != "/opt/geoip/GeoLite2-City.mmdb" {
		t.Errorf("GeoIPDBPath: got %q", cfg.GeoIPDBPath)
	}
}

// Anything other than the exact opt-out keeps enforcement ON — a typo must not
// silently disable a compliance control.
func TestLoad_GeofenceModeTypoDoesNotDisable(t *testing.T) {
	for _, mode := range []string{"disable", "off", "false", "no"} {
		t.Run(mode, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv("GEOFENCE_MODE", mode)
			t.Setenv("GEOIP_DB_PATH", "")

			if _, err := Load(); err == nil {
				t.Fatalf("GEOFENCE_MODE=%q must NOT disable enforcement", mode)
			}
		})
	}
}

func TestLoad_TrustedProxiesParsed(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("TRUSTED_PROXIES", "10.0.0.0/8, 172.16.0.0/12 ,192.168.1.1")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.1.1"}
	if len(cfg.TrustedProxies) != len(want) {
		t.Fatalf("TrustedProxies: got %v want %v", cfg.TrustedProxies, want)
	}
	for i := range want {
		if cfg.TrustedProxies[i] != want[i] {
			t.Errorf("TrustedProxies[%d]: got %q want %q", i, cfg.TrustedProxies[i], want[i])
		}
	}
}

// The default is EMPTY — X-Forwarded-For is ignored unless a proxy is
// explicitly trusted, so no deployment accidentally honours a spoofable header.
func TestLoad_TrustedProxiesDefaultsEmpty(t *testing.T) {
	setRequiredEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.TrustedProxies) != 0 {
		t.Errorf("TrustedProxies must default to empty, got %v", cfg.TrustedProxies)
	}
}

// ----------------------------------------------------------------------------
// Retired legacy-HMAC kill switch
// ----------------------------------------------------------------------------

// TestLoad_RetiredLegacyHMACSwitchIsFatal proves the boot fails when a
// deployment still asks for the removed body-only signature path.
//
// Silently ignoring the variable would leave a gap between what the manifest
// says and what the engine does: the operator believes legacy signatures are
// accepted, their integrators keep signing the old way, and the only symptom
// is a flood of 401s in production with no obvious cause. Failing here moves
// that discovery to deploy time.
//
// Note the absence of a Config field to assert against — the flag was deleted
// from the struct, so "the legacy path cannot be re-enabled" is enforced by
// the compiler. This test covers the remaining runtime surface: the variable
// itself.
func TestLoad_RetiredLegacyHMACSwitchIsFatal(t *testing.T) {
	// Values that read as a request to turn the legacy path ON. "1" and "yes"
	// are included deliberately: the old parser treated them as false, so a
	// deployment using them was ALREADY not getting the behaviour it asked
	// for. That silent mismatch is exactly what must now fail loudly.
	fatal := []string{"true", "TRUE", "True", " true ", "1", "yes", "on"}
	for _, v := range fatal {
		t.Run("fatal_"+v, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(legacyHMACEnvVar, v)

			_, err := Load()
			if err == nil {
				t.Fatalf("%s=%q must fail the boot", legacyHMACEnvVar, v)
			}
			if !strings.Contains(err.Error(), legacyHMACEnvVar) {
				t.Errorf("error must name the offending variable, got: %v", err)
			}
		})
	}

	// An explicitly falsy leftover expects the secure behaviour and gets it,
	// so upgrading must not take that deployment down over a dead config line.
	inert := []string{"", "false", "FALSE", "0", "no", "off", "  false  "}
	for _, v := range inert {
		t.Run("inert_"+v, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(legacyHMACEnvVar, v)

			if _, err := Load(); err != nil {
				t.Fatalf("%s=%q expects the secure path and must boot: %v",
					legacyHMACEnvVar, v, err)
			}
		})
	}

	// Unset is the steady state once the variable has been cleaned up.
	t.Run("unset", func(t *testing.T) {
		setRequiredEnv(t)
		if _, err := Load(); err != nil {
			t.Fatalf("unset %s must boot: %v", legacyHMACEnvVar, err)
		}
	})
}
