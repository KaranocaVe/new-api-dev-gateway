// Package gateway configures an unmodified, official New API Linux binary.
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
)

type Config struct {
	Version        int               `json:"version"`
	Port           int               `json:"port"`
	Retries        int               `json:"retries"`
	ClientAllowIPs []string          `json:"client_allow_ips"`
	Runtime        map[string]string `json:"runtime"`
	Channels       []Channel         `json:"channels"`
}

type Channel struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Type               string   `json:"type"`
	BaseURL            string   `json:"base_url"`
	Key                string   `json:"key"`
	Models             []string `json:"models"`
	Priority           int      `json:"priority"`
	Weight             *int     `json:"weight,omitempty"`
	Enabled            *bool    `json:"enabled,omitempty"`
	ResponsesWebsocket bool     `json:"responses_websocket,omitempty"`
}

var runtimeDefaults = map[string]string{
	"GOMEMLIMIT": "256MiB", "GOGC": "100",
	"SQL_MAX_IDLE_CONNS": "1", "SQL_MAX_OPEN_CONNS": "4", "SQL_MAX_LIFETIME": "0",
	"RELAY_MAX_IDLE_CONNS": "32", "RELAY_MAX_IDLE_CONNS_PER_HOST": "8", "RELAY_IDLE_CONN_TIMEOUT": "90",
	"RELAY_TIMEOUT": "0", "RELAY_RESPONSE_HEADER_TIMEOUT": "1800", "STREAMING_TIMEOUT": "300",
	"MAX_REQUEST_BODY_MB": "32", "STREAM_SCANNER_MAX_BUFFER_MB": "32", "SHUTDOWN_TIMEOUT_SECONDS": "30",
}

func decodeStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	// Do not include decoder errors: an unknown JSON field may itself be a secret.
	if d.Decode(v) != nil {
		return errors.New("invalid JSON or unknown field; see examples/gateway.json")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected exactly one JSON document")
	}
	return nil
}

func readConfig(path string) (Config, error) {
	c := Config{Port: 3000, Retries: 2, ClientAllowIPs: []string{"127.0.0.1", "::1"}}
	st, err := os.Stat(path)
	if err != nil {
		return c, errors.New("cannot read config file")
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return c, errors.New("config must be a regular private file (chmod 600)")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, errors.New("cannot read config file")
	}
	if err = decodeStrict(b, &c); err != nil {
		return c, err
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if c.Version != 1 {
		return errors.New("config version must be 1")
	}
	if c.Port < 1024 || c.Port > 65535 {
		return errors.New("port must be between 1024 and 65535")
	}
	if c.Retries < 0 || c.Retries > 5 {
		return errors.New("retries must be between 0 and 5")
	}
	if len(c.Channels) == 0 {
		return errors.New("at least one channel is required")
	}
	if len(c.ClientAllowIPs) == 0 {
		return errors.New("client_allow_ips must contain an IP or CIDR")
	}
	for _, s := range c.ClientAllowIPs {
		if net.ParseIP(s) == nil {
			if _, _, e := net.ParseCIDR(s); e != nil {
				return errors.New("invalid client_allow_ips")
			}
		}
	}
	for k, v := range c.Runtime {
		if _, ok := runtimeDefaults[k]; !ok {
			return errors.New("unsupported runtime variable; see README")
		}
		if k == "GOMEMLIMIT" {
			if !regexp.MustCompile(`^[1-9][0-9]*(KiB|MiB|GiB|B)$|^off$`).MatchString(v) {
				return errors.New("invalid GOMEMLIMIT")
			}
		} else if !regexp.MustCompile(`^[0-9]{1,7}$`).MatchString(v) {
			return errors.New("runtime values must be non-negative integers")
		}
		if (k == "SQL_MAX_OPEN_CONNS" || k == "MAX_REQUEST_BODY_MB" || k == "STREAM_SCANNER_MAX_BUFFER_MB" || k == "STREAMING_TIMEOUT") && v == "0" {
			return errors.New("runtime pool/body/stream limits must be positive")
		}
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for i, ch := range c.Channels {
		bad := func(s string) error { return fmt.Errorf("channel %d: %s", i+1, s) }
		if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`).MatchString(ch.ID) || ids[ch.ID] {
			return bad("id must be unique and contain letters, digits, underscores or hyphens")
		}
		ids[ch.ID] = true
		if strings.TrimSpace(ch.Name) == "" || len(ch.Name) > 100 || names[ch.Name] {
			return bad("name must be nonempty and unique (max 100 bytes)")
		}
		names[ch.Name] = true
		if ch.Type != "openai" && ch.Type != "anthropic" {
			return bad("type must be openai or anthropic")
		}
		u, e := url.Parse(ch.BaseURL)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return bad("invalid base_url (no credentials, query or fragment)")
		}
		if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/v1") {
			return bad("base_url must omit the final /v1; New API appends the endpoint")
		}
		if strings.TrimSpace(ch.Key) == "" || strings.ContainsAny(ch.Key, "\r\n") {
			return bad("key must be a nonempty single-line credential")
		}
		if len(ch.Models) == 0 {
			return bad("models cannot be empty")
		}
		seen := map[string]bool{}
		for _, m := range ch.Models {
			if m == "" || strings.ContainsAny(m, ",\r\n") || strings.TrimSpace(m) != m || seen[m] {
				return bad("models must be nonempty unique IDs without commas or whitespace at either end")
			}
			seen[m] = true
		}
		if ch.Priority < 0 || ch.Priority > 1000000 {
			return bad("priority must be between 0 and 1000000")
		}
		if ch.Weight != nil && (*ch.Weight < 1 || *ch.Weight > 1000) {
			return bad("weight must be between 1 and 1000")
		}
	}
	if len(c.models()) == 0 {
		return errors.New("at least one enabled model is required")
	}
	return nil
}

func (c Config) models() []string {
	m := map[string]bool{}
	for _, ch := range c.Channels {
		if ch.Enabled == nil || *ch.Enabled {
			for _, s := range ch.Models {
				m[s] = true
			}
		}
	}
	out := make([]string, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (ch Channel) payload() map[string]any {
	t := 1
	if ch.Type == "anthropic" {
		t = 14
	}
	w := 1
	if ch.Weight != nil {
		w = *ch.Weight
	}
	setting, _ := json.Marshal(map[string]any{"pass_through_body_enabled": ch.Type == "openai", "responses_websocket_enabled": ch.ResponsesWebsocket})
	settings, _ := json.Marshal(map[string]any{"upstream_model_update_check_enabled": false, "upstream_model_update_auto_sync_enabled": false})
	return map[string]any{"type": t, "name": ch.Name, "key": ch.Key, "base_url": strings.TrimRight(ch.BaseURL, "/"), "models": strings.Join(ch.Models, ","), "group": "default", "priority": ch.Priority, "weight": w, "auto_ban": 0, "setting": string(setting), "settings": string(settings)}
}
