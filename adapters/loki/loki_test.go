package loki

import (
	"os"
	"testing"
)

func TestSchemeDefault(t *testing.T) {
	if got := scheme("loki"); got != "http" {
		t.Errorf("expected 'http' for adapter 'loki', got '%s'", got)
	}
}

func TestSchemeHTTPS(t *testing.T) {
	if got := scheme("loki+https"); got != "https" {
		t.Errorf("expected 'https' for adapter 'loki+https', got '%s'", got)
	}
}

func TestSchemeHTTP(t *testing.T) {
	if got := scheme("loki+http"); got != "http" {
		t.Errorf("expected 'http' for adapter 'loki+http', got '%s'", got)
	}
}

func TestGetHostnameFromEnv(t *testing.T) {
	os.Setenv("SYSLOG_HOSTNAME", "myhost.example.com")
	defer os.Unsetenv("SYSLOG_HOSTNAME")

	// Remove any host_hostname file influence by ensuring it doesn't exist at tmp path.
	// getHostname reads /etc/host_hostname; on test machines this typically doesn't exist,
	// so the env var fallback is used.
	got := getHostname()
	if got != "myhost.example.com" {
		// Only fail if /etc/host_hostname also doesn't exist (normal CI/dev case).
		if _, err := os.Stat("/etc/host_hostname"); os.IsNotExist(err) {
			t.Errorf("expected hostname 'myhost.example.com' from env, got '%s'", got)
		}
	}
}

func TestGetHostnameDefault(t *testing.T) {
	os.Unsetenv("SYSLOG_HOSTNAME")
	got := getHostname()
	// When neither /etc/host_hostname nor SYSLOG_HOSTNAME is set, the default
	// template string is returned.
	if _, err := os.Stat("/etc/host_hostname"); os.IsNotExist(err) {
		expected := "{{.Container.Config.Hostname}}"
		if got != expected {
			t.Errorf("expected default template '%s', got '%s'", expected, got)
		}
	}
}
