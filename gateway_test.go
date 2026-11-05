package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configForTest() Config {
	return Config{Version: 1, Port: 3000, Retries: 2, ClientAllowIPs: []string{"127.0.0.1"}, Channels: []Channel{{ID: "primary", Name: "Primary", Type: "openai", BaseURL: "https://api.example.com", Key: "private-example-key", Models: []string{"gpt-5.4"}}}}
}

func TestConfigBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Config)
	}{
		{"url credentials", func(c *Config) { c.Channels[0].BaseURL = "https://secret:password@api.example.com" }},
		{"duplicate identity", func(c *Config) { c.Channels = append(c.Channels, c.Channels[0]) }},
		{"model delimiter", func(c *Config) { c.Channels[0].Models = []string{"a,b"} }},
		{"unsafe env", func(c *Config) { c.Runtime = map[string]string{"SQL_DSN": "secret"} }},
		{"unknown provider", func(c *Config) { c.Channels[0].Type = "codex-oauth" }},
		{"endpoint instead of origin", func(c *Config) { c.Channels[0].BaseURL += "/v1" }},
		{"empty client restriction", func(c *Config) { c.ClientAllowIPs = nil }},
		{"all disabled", func(c *Config) { b := false; c.Channels[0].Enabled = &b }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := configForTest()
			tt.change(&c)
			if c.validate() == nil {
				t.Fatal("accepted unsafe config")
			}
		})
	}
	c := configForTest()
	if e := c.validate(); e != nil {
		t.Fatal(e)
	}
}

func TestPrivateJSONAndRedaction(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	b, _ := json.Marshal(configForTest())
	if e := os.WriteFile(p, b, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := readConfig(p); e == nil {
		t.Fatal("read public secret file")
	}
	os.Chmod(p, 0600)
	if _, e := readConfig(p); e != nil {
		t.Fatal(e)
	}
	b = []byte(`{"secret-field-name":"private-example-key"}`)
	os.WriteFile(p, b, 0600)
	if _, e := readConfig(p); e == nil || strings.Contains(e.Error(), "secret-field-name") || strings.Contains(e.Error(), "private-example-key") {
		t.Fatal("invalid config disclosed content")
	}
	if e := decodeStrict([]byte(`{} {}`), new(Config)); e == nil {
		t.Fatal("accepted two documents")
	}
}

func TestEnvironmentAndChannelPolicy(t *testing.T) {
	t.Setenv("SQL_DSN", "must-not-be-used")
	t.Setenv("REDIS_CONN_STRING", "must-not-be-used")
	t.Setenv("PORT", "9999")
	c := configForTest()
	c.Runtime = map[string]string{"GOMEMLIMIT": "512MiB"}
	env := strings.Join(environment(c, "/state", &State{SessionSecret: "generated"}, 3000), "\n")
	for _, v := range []string{"PORT=3000", "GOMEMLIMIT=512MiB", "UPDATE_TASK=false", "CHANNEL_UPSTREAM_MODEL_UPDATE_TASK_ENABLED=false", "TRUSTED_PROXIES=none", "_pragma=journal_mode(WAL)"} {
		if !strings.Contains(env, v) {
			t.Fatal("missing runtime policy", v)
		}
	}
	if strings.Contains(env, "must-not-be-used") {
		t.Fatal("inherited external database config")
	}
	p := c.Channels[0].payload()
	var settings map[string]any
	json.Unmarshal([]byte(p["setting"].(string)), &settings)
	if p["type"] != 1 || settings["pass_through_body_enabled"] != true || p["tag"] != nil || p["param_override"] != nil {
		t.Fatal("unexpected channel policy")
	}
	opts := leanOptions(c)
	if opts["GroupRatio"] != `{"default":0}` || opts["quota_setting.enable_free_model_pre_consume"] != "false" {
		t.Fatal("local quota was not disabled through zero rating")
	}
}

func TestAPIErrorNeverIncludesBody(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"success":false,"message":"private-example-key"}`)
	}))
	defer s.Close()
	a := newAPI(3000)
	a.base = s.URL
	if e := a.call("GET", "/api/option/", nil, nil); e == nil || strings.Contains(e.Error(), "private-example-key") {
		t.Fatal("API leaked error body")
	}
}

func TestStateAndPendingCreateRecovery(t *testing.T) {
	dir := t.TempDir()
	if e := initDirs(dir); e != nil {
		t.Fatal(e)
	}
	s, e := loadState(dir, true)
	if e != nil {
		t.Fatal(e)
	}
	s2, e := loadState(dir, true)
	if e != nil || s.Password != s2.Password || s.SessionSecret != s2.SessionSecret {
		t.Fatal("credentials rotated on restart")
	}
	// Simulate a crash after POST /channel but before saving its returned id.
	s.Pending["primary"] = "Primary"
	s.Pending["removed"] = "Removed before retry"
	created := 0
	updated := 0
	disabled := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var data any
		switch {
		case r.URL.Path == "/api/option/" && r.Method == "GET":
			values := []map[string]string{}
			for k, v := range leanOptions(configForTest()) {
				values = append(values, map[string]string{"key": k, "value": v})
			}
			data = values
		case r.URL.Path == "/api/channel/" && r.Method == "GET":
			data = map[string]any{"total": 2, "items": []map[string]any{{"id": 42, "name": "Primary", "type": 1}, {"id": 43, "name": "Removed before retry", "type": 1}}}
		case r.URL.Path == "/api/channel/" && r.Method == "POST":
			created++
		case r.URL.Path == "/api/channel/" && r.Method == "PUT":
			updated++
		case r.URL.Path == "/api/channel/43/status":
			var status struct {
				Status int `json:"status"`
			}
			json.NewDecoder(r.Body).Decode(&status)
			if status.Status == 2 {
				disabled++
			}
		case r.URL.Path == "/api/token/" && r.Method == "GET":
			data = map[string]any{"total": 1, "items": []map[string]any{{"id": 7, "name": s.TokenName}}}
		case r.URL.Path == "/api/token/7/key":
			data = map[string]string{"key": "synthetic-client-key"}
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}))
	defer server.Close()
	a := newAPI(3000)
	a.base = server.URL
	if e := a.sync(configForTest(), dir, s); e != nil {
		t.Fatal(e)
	}
	if created != 0 || updated != 1 || disabled != 1 || s.Managed["primary"] != 42 || s.Managed["removed"] != 43 || len(s.Pending) != 0 || s.TokenID != 7 {
		t.Fatal("did not recover pending create")
	}
	st, _ := os.Stat(filepath.Join(dir, "client.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatal("credentials are public")
	}
}
