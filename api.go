package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"sort"
	"strconv"
	"strings"
	"time"
)

type api struct {
	base, token string
	client      *http.Client
}

func newAPI(port int) *api {
	jar, _ := cookiejar.New(nil)
	return &api{base: fmt.Sprintf("http://127.0.0.1:%d", port), client: &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (a *api) call(method, path string, body any, out any) error {
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, a.base+path, bytes.NewReader(b))
	if err != nil {
		return errors.New("invalid API request")
	}
	req.Header.Set("Content-Type", "application/json")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: connection failed", method, strings.Split(path, "?")[0])
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("%s %s: HTTP %d", method, strings.Split(path, "?")[0], resp.StatusCode)
	}
	var result struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&result) != nil || !result.Success {
		return fmt.Errorf("%s %s: New API rejected the request (response redacted)", method, strings.Split(path, "?")[0])
	}
	if out != nil && json.Unmarshal(result.Data, out) != nil {
		return fmt.Errorf("%s: incompatible API response", path)
	}
	return nil
}

func (a *api) login(s *State) error {
	var r struct {
		Token string `json:"access_token"`
	}
	if err := a.call("POST", "/api/user/login", map[string]string{"username": s.Username, "password": s.Password}, &r); err != nil {
		return err
	}
	if r.Token == "" {
		return errors.New("upstream login returned no session token")
	}
	a.token = r.Token
	return nil
}
func (a *api) logout() {
	if a.token != "" {
		_ = a.call("POST", "/api/user/auth/logout", map[string]any{}, nil)
		a.token = ""
	}
}

func leanOptions(c Config) map[string]string {
	opts := map[string]string{
		"SelfUseModeEnabled": "true", "DemoSiteEnabled": "false",
		"RegisterEnabled": "false", "PasswordRegisterEnabled": "false", "PasswordLoginEnabled": "true",
		"EmailVerificationEnabled": "false", "GitHubOAuthEnabled": "false", "LinuxDOOAuthEnabled": "false", "TelegramOAuthEnabled": "false", "WeChatAuthEnabled": "false", "TurnstileCheckEnabled": "false",
		"DrawingEnabled": "false", "TaskEnabled": "false", "TaskPluginEnabled": "false", "DataExportEnabled": "false", "LogConsumeEnabled": "false", "QuotaRemindEnabled": "false",
		"CheckSensitiveEnabled": "false", "CheckSensitiveOnPromptEnabled": "false", "StopOnSensitiveEnabled": "false",
		"AutomaticDisableChannelEnabled": "false", "AutomaticEnableChannelEnabled": "false", "ModelRequestRateLimitEnabled": "false",
		"WaffoEnabled": "false", "checkin_setting.enabled": "false", "monitor_setting.auto_test_channel_enabled": "false",
		"performance_setting.monitor_enabled": "false", "performance_setting.disk_cache_enabled": "false",
		"console_setting.api_info_enabled": "false", "console_setting.uptime_kuma_enabled": "false", "console_setting.announcements_enabled": "false", "console_setting.faq_enabled": "false",
		"global.pass_through_request_enabled": "false", "global.chat_completions_to_responses_policy": `{"enabled":false,"all_channels":false}`,
		"quota_setting.enable_free_model_pre_consume": "false", "GroupRatio": `{"default":0}`, "RetryTimes": strconv.Itoa(c.Retries),
	}
	// SelfUseMode only permits models without prices. Explicit zero prices and ratio
	// mode avoid local wallet depletion, including built-in tiered model prices.
	prices, modes := map[string]int{}, map[string]string{}
	for _, m := range c.models() {
		prices[m] = 0
		modes[m] = "ratio"
	}
	for k, v := range map[string]any{"ModelPrice": prices, "ModelRatio": prices, "billing_setting.billing_mode": modes} {
		b, _ := json.Marshal(v)
		opts[k] = string(b)
	}
	return opts
}

type channelRecord struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type int    `json:"type"`
}

func (a *api) channels() ([]channelRecord, error) {
	var out []channelRecord
	for p := 1; ; p++ {
		var r struct {
			Items []channelRecord `json:"items"`
			Total int             `json:"total"`
		}
		if e := a.call("GET", fmt.Sprintf("/api/channel/?p=%d&size=100", p), nil, &r); e != nil {
			return nil, e
		}
		out = append(out, r.Items...)
		if len(out) >= r.Total {
			return out, nil
		}
		if len(r.Items) == 0 {
			return nil, errors.New("invalid channel pagination")
		}
	}
}

func (a *api) sync(c Config, dir string, s *State) error {
	opts := leanOptions(c)
	var existing []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if e := a.call("GET", "/api/option/", nil, &existing); e != nil {
		return e
	}
	available := map[string]string{}
	for _, o := range existing {
		available[o.Key] = o.Value
	}
	keys := make([]string, 0, len(opts))
	for k := range opts {
		if _, ok := available[k]; !ok {
			return fmt.Errorf("upstream does not expose required option %s", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if available[k] != opts[k] {
			if e := a.call("PUT", "/api/option/", map[string]string{"key": k, "value": opts[k]}, nil); e != nil {
				return fmt.Errorf("option %s: %w", k, e)
			}
		}
	}
	records, e := a.channels()
	if e != nil {
		return e
	}
	byID := map[int]channelRecord{}
	for _, r := range records {
		byID[r.ID] = r
	}
	// Recover every persisted creation intent, even if its channel was removed
	// from JSON after a crash. Recovered removed channels must still be disabled.
	for logical, name := range s.Pending {
		id := 0
		for _, r := range records {
			if r.Name == name {
				if id != 0 {
					return errors.New("ambiguous pending channel; inspect state and dashboard")
				}
				id = r.ID
			}
		}
		if id != 0 {
			s.Managed[logical] = id
			delete(s.Pending, logical)
			if e = saveState(dir, s); e != nil {
				return e
			}
		}
	}
	desired := map[string]bool{}
	for _, ch := range c.Channels {
		desired[ch.ID] = true
		id := s.Managed[ch.ID]
		payload := ch.payload()
		if id == 0 {
			// Never adopt unrelated channels just because their names match.
			for _, r := range records {
				if r.Name == ch.Name {
					return errors.New("unmanaged channel already has a configured name; choose another name")
				}
			}
			if id == 0 {
				s.Pending[ch.ID] = ch.Name
				if e = saveState(dir, s); e != nil {
					return e
				}
				payload["status"] = 1
				var r struct {
					ID int `json:"id"`
				}
				if e = a.call("POST", "/api/channel/", map[string]any{"mode": "single", "batch_add_set_key_prefix_2_name": false, "channel": payload}, &r); e != nil {
					return e
				}
				id = r.ID
				if id <= 0 {
					return errors.New("upstream returned no channel id")
				}
			}
			s.Managed[ch.ID] = id
			delete(s.Pending, ch.ID)
			if e = saveState(dir, s); e != nil {
				return e
			}
		} else if _, ok := byID[id]; !ok {
			return errors.New("managed channel was deleted outside JSON; remove its mapping from state.json while stopped to recreate it")
		}
		payload["id"] = id
		delete(payload, "status")
		if e = a.call("PUT", "/api/channel/", payload, nil); e != nil {
			return e
		}
		status := 1
		if ch.Enabled != nil && !*ch.Enabled {
			status = 2
		}
		if e = a.call("POST", fmt.Sprintf("/api/channel/%d/status", id), map[string]int{"status": status}, nil); e != nil {
			return e
		}
	}
	for logical, id := range s.Managed {
		if !desired[logical] {
			if _, ok := byID[id]; ok {
				if e = a.call("POST", fmt.Sprintf("/api/channel/%d/status", id), map[string]int{"status": 2}, nil); e != nil {
					return e
				}
			}
		}
	}
	return a.syncToken(c, dir, s)
}

func (a *api) syncToken(c Config, dir string, s *State) error {
	payload := map[string]any{"name": s.TokenName, "expired_time": -1, "unlimited_quota": true, "group": "default", "allow_ips": strings.Join(c.ClientAllowIPs, "\n"), "model_limits_enabled": true, "model_limits": strings.Join(c.models(), ","), "allow_user_quota_query": false}
	if s.TokenID == 0 {
		// A unique persisted name also recovers an interrupted token creation.
		var r struct {
			Items []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"items"`
			Total int `json:"total"`
		}
		for p := 1; ; p++ {
			if e := a.call("GET", fmt.Sprintf("/api/token/?p=%d&size=100", p), nil, &r); e != nil {
				return e
			}
			for _, t := range r.Items {
				if t.Name == s.TokenName {
					if s.TokenID != 0 {
						return errors.New("ambiguous client token name")
					}
					s.TokenID = t.ID
				}
			}
			if p*100 >= r.Total {
				break
			}
		}
		if s.TokenID == 0 {
			if e := a.call("POST", "/api/token/", payload, nil); e != nil {
				return e
			}
			if e := a.call("GET", "/api/token/?p=1&size=100", nil, &r); e != nil {
				return e
			}
			for _, t := range r.Items {
				if t.Name == s.TokenName {
					s.TokenID = t.ID
				}
			}
		}
		if s.TokenID == 0 {
			return errors.New("cannot find created client token")
		}
		if e := saveState(dir, s); e != nil {
			return e
		}
	}
	payload["id"] = s.TokenID
	if e := a.call("PUT", "/api/token/", payload, nil); e != nil {
		return e
	}
	var key struct {
		Key string `json:"key"`
	}
	if e := a.call("POST", fmt.Sprintf("/api/token/%d/key", s.TokenID), map[string]any{}, &key); e != nil {
		return e
	}
	if key.Key == "" {
		return errors.New("upstream returned an empty client key")
	}
	return writeJSON(dir+"/client.json", map[string]string{"base_url": fmt.Sprintf("http://127.0.0.1:%d/v1", c.Port), "api_key": key.Key})
}
