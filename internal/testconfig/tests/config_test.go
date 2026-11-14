package testconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExampleContainsNoLiveCredentials(t *testing.T) {
	config, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if config.Username != "" || config.Password != "" || config.Fixtures.Username != "" || config.Fixtures.Password != "" {
		t.Fatal("example must not include credentials")
	}
	for _, camera := range config.Cameras {
		if camera.Host != "" {
			t.Fatal("example must not include real camera addresses")
		}
	}
	if config.ValidateLive() == nil {
		t.Fatal("blank live configuration must fail before contacting cameras")
	}
}

func TestLoadRejectsUnknownFieldsWithoutEchoingInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	marker := Password()
	if err := os.WriteFile(path, []byte(`{"unexpected":"`+marker+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Fatal("invalid configuration must fail without exposing input")
	}
}

func TestFixtureExpansionUsesConfiguredValues(t *testing.T) {
	if Expand("{{host2}} {{username}} {{password}}") != Host(2)+" "+Username()+" "+Password() {
		t.Fatal("fixture expansion did not use configured values")
	}
	if Username() == "" || Password() == "" {
		t.Fatal("blank fixture credentials must be generated in memory")
	}
}

func TestLiveConfigurationValidatesSelectedCameras(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
		valid  bool
	}{
		{name: "configured network", change: func(*Config) {}, valid: true},
		{name: "blank username", change: func(config *Config) { config.Username = "" }},
		{name: "blank password", change: func(config *Config) { config.Password = "" }},
		{name: "public address", change: func(config *Config) { config.Cameras[0].Host = "8.8.8.8" }},
		{name: "blank address", change: func(config *Config) { config.Cameras[0].Host = "" }},
		{name: "invalid port", change: func(config *Config) { config.Cameras[0].Port = 65536 }},
		{name: "invalid channel", change: func(config *Config) { config.Cameras[0].Channel = -1 }},
		{name: "duplicate ID", change: func(config *Config) { config.Cameras[1].ID = config.Cameras[0].ID }},
		{name: "missing probe", change: func(config *Config) { config.ProbeCamera = "missing" }},
		{name: "missing storage", change: func(config *Config) { config.StorageCamera = "missing" }},
		{name: "too few cameras", change: func(config *Config) { config.Cameras = config.Cameras[:1] }},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			config, err := Load("")
			if err != nil {
				t.Fatal(err)
			}
			config.Username, config.Password = Username(), Password()
			for index := range config.Cameras {
				config.Cameras[index].Host = Host(index + 1)
			}
			item.change(&config)
			if (config.ValidateLive() == nil) != item.valid {
				t.Fatal("unexpected live configuration validation result")
			}
		})
	}
}
