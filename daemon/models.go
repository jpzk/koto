package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// Model catalog for the TUI's new-group dialog.
//
// The list is fetched HOST-side with the host credential, through the same
// upstream origins the proxy relays to, and handed to the operator's client
// only (the RPC is operator-scoped in acl.go; guests never reach it). Every
// string in it came from a third party and ends up on a terminal, so it is
// treated as untrusted: ids that do not fit a conservative identifier
// charset are dropped, display names lose control characters and escape
// sequences and are bounded, and the list itself is capped.
//
// When there is no credential or the upstream fails, the answer is a small
// built-in list with source "fallback" and the reason, so the dialog always
// has something to offer.

const (
	modelsTTL         = 10 * time.Minute
	modelsFallbackTTL = 30 * time.Second // retry soon after a credential fix
	modelsTimeout     = 5 * time.Second
	modelsMaxEntries  = 100
	modelsMaxName     = 64
	modelsMaxBody     = 4 << 20
	modelsMaxPages    = 10
	modelsPageLimit   = 100
	// modelsClientVersion is the codex release koto pins (rootfs); the
	// ChatGPT backend's catalog is keyed on it (codex-api endpoint/models.rs).
	modelsClientVersion = "0.152.1"
)

const (
	modelsSourceLive     = "live"
	modelsSourceFallback = "fallback"
)

// modelIDRE is the charset a model id must fit to be offered at all.
var modelIDRE = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,64}$`)

// validModelID reports whether s is an id the picker may offer or accept:
// the picker charset AND the config/spawn identifier rule, so anything shown
// is also something seedSpawnConfig will take.
func validModelID(s string) bool {
	return modelIDRE.MatchString(s) && configIdentRE.MatchString(s)
}

type modelEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

type modelList struct {
	Provider string       `json:"provider"`
	Source   string       `json:"source"`
	Error    string       `json:"error,omitempty"`
	Models   []modelEntry `json:"models"`
}

// modelsFallback is the offline list per provider: the defaults koto itself
// runs plus a few current siblings. Kept short on purpose; "custom…" in the
// dialog covers everything else.
var modelsFallback = map[string][]modelEntry{
	"claudesdk": {
		{defaultClaudeModel, "Claude Sonnet 5"},
		{"claude-opus-5", "Claude Opus 5"},
		{"claude-haiku-4-5", "Claude Haiku 4.5"},
	},
	"venice": {
		{defaultVeniceModel, "Kimi K2.5"},
		{"llama-3.3-70b", "Llama 3.3 70B"},
	},
	"openrouter": {
		{defaultOpenRouterModel, "Kimi K3"},
		{"z-ai/glm-5.3", "GLM 5.3"},
		{"moonshotai/kimi-k2.5", "Kimi K2.5"},
	},
	"openai": {
		{defaultOpenAIModel, "GPT-5.5"},
	},
}

// Upstream origins, read at call time so an override of the proxy's origin
// (KOTO_UPSTREAM) applies here too. Tests replace these.
var (
	modelsAnthropicBase  = func() string { return upstream }
	modelsVeniceBase     = func() string { return veniceUpstream }
	modelsOpenRouterBase = func() string { return openrouterUpstream }
	modelsOpenAIBase     = func(kind string) string {
		if kind == "chatgpt" {
			return openaiChatGPTUpstream
		}
		return openaiAPIUpstream
	}
	modelsHTTP = &http.Client{Timeout: modelsTimeout}
)

// Credential lookups, indirected for tests.
var (
	modelsAnthropicAuth  = authHeaders
	modelsVeniceAuth     = veniceAuth
	modelsOpenRouterAuth = openrouterAuth
	modelsOpenAIAuth     = openaiCredential
)

// ---- sanitizer -------------------------------------------------------------

// escSeqRE matches ANSI/VT escape sequences: CSI, OSC/DCS/SOS/PM/APC strings
// (BEL- or ST-terminated, or unterminated to end of string), and two-byte
// escapes. 8-bit C1 introducers (U+009B CSI, U+009D OSC) are covered too.
var escSeqRE = regexp.MustCompile(
	"(?:\x1b\\[|\u009b)[0-?]*[ -/]*[@-~]?" +
		"|(?:\x1b[\\]PX^_]|[\u009d\u0090\u0098\u009e\u009f])[^\x07\x1b\u009c]*(?:\x07|\x1b\\\\|\u009c)?" +
		"|\x1b[ -~]?")

// sanitizeModelName strips escape sequences, control and format characters
// (which includes bidi overrides), collapses whitespace and bounds the
// result to modelsMaxName runes.
func sanitizeModelName(s string) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = escSeqRE.ReplaceAllString(s, "")
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			space = true
			continue
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), r == utf8.RuneError:
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	out := b.String()
	if utf8.RuneCountInString(out) > modelsMaxName {
		rs := []rune(out)
		out = strings.TrimSpace(string(rs[:modelsMaxName-1])) + "…"
	}
	return out
}

// sanitizeModels drops invalid ids, dedupes, cleans names, sorts by id and
// caps the list.
func sanitizeModels(in []modelEntry) []modelEntry {
	seen := map[string]bool{}
	out := make([]modelEntry, 0, len(in))
	for _, m := range in {
		if !validModelID(m.ID) || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		name := sanitizeModelName(m.DisplayName)
		if name == "" {
			name = m.ID
		}
		out = append(out, modelEntry{ID: m.ID, DisplayName: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > modelsMaxEntries {
		out = out[:modelsMaxEntries]
	}
	return out
}

// ---- fetchers --------------------------------------------------------------

// modelsGet issues one GET and decodes a bounded JSON body into v. Upstream
// error bodies are not echoed (they can carry account detail); only the
// status goes back.
func modelsGet(ctx context.Context, u string, hdr map[string]string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, val := range hdr {
		req.Header.Set(k, val)
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("accept-encoding", "identity")
	resp, err := modelsHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("upstream timed out after %s", modelsTimeout)
		}
		return errors.New("upstream unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, modelsMaxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("upstream timed out after %s", modelsTimeout)
		}
		return errors.New("reading the upstream response failed")
	}
	if len(body) > modelsMaxBody {
		return errors.New("upstream response too large")
	}
	if err := json.Unmarshal(body, v); err != nil {
		return errors.New("upstream response is not a model list")
	}
	return nil
}

// fetchAnthropicModels: GET /v1/models, paginated with after_id while
// has_more, bounded by modelsMaxPages.
func fetchAnthropicModels(ctx context.Context) ([]modelEntry, error) {
	hdr, err := modelsAnthropicAuth()
	if err != nil {
		return nil, errors.New("no anthropic credential on this host")
	}
	base := strings.TrimRight(modelsAnthropicBase(), "/")
	var all []modelEntry
	after := ""
	for page := 0; page < modelsMaxPages; page++ {
		q := url.Values{"limit": {fmt.Sprint(modelsPageLimit)}}
		if after != "" {
			q.Set("after_id", after)
		}
		var r struct {
			Data []struct {
				ID          string `json:"id"`
				DisplayName string `json:"display_name"`
			} `json:"data"`
			HasMore bool   `json:"has_more"`
			LastID  string `json:"last_id"`
		}
		if err := modelsGet(ctx, base+"/v1/models?"+q.Encode(), hdr, &r); err != nil {
			return nil, err
		}
		for _, d := range r.Data {
			all = append(all, modelEntry{d.ID, d.DisplayName})
		}
		if !r.HasMore || r.LastID == "" || r.LastID == after || len(all) >= modelsMaxEntries*modelsMaxPages {
			break
		}
		after = r.LastID
	}
	return all, nil
}

// fetchVeniceModels: GET /api/v1/models (text models by default).
func fetchVeniceModels(ctx context.Context) ([]modelEntry, error) {
	key, err := modelsVeniceAuth()
	if err != nil || key == "" {
		return nil, errors.New("no venice credential on this host")
	}
	var r struct {
		Data []struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			ModelSpec struct {
				Name string `json:"name"`
			} `json:"model_spec"`
		} `json:"data"`
	}
	u := strings.TrimRight(modelsVeniceBase(), "/") + "/api/v1/models"
	if err := modelsGet(ctx, u, map[string]string{"Authorization": "Bearer " + key}, &r); err != nil {
		return nil, err
	}
	var out []modelEntry
	for _, d := range r.Data {
		if d.Type != "" && d.Type != "text" {
			continue
		}
		out = append(out, modelEntry{d.ID, d.ModelSpec.Name})
	}
	return out, nil
}

// openaiNonChat marks API-key catalog entries that cannot drive a Codex turn.
var openaiNonChat = []string{"embedding", "tts", "whisper", "dall-e", "moderation",
	"transcribe", "audio", "realtime", "image", "davinci", "babbage", "sora"}

// fetchOpenAIModels: ChatGPT mode GET {codex base}/models?client_version=
// (the endpoint codex 0.152.1 itself uses, already on the proxy allowlist);
// API-key mode GET {api base}/models, minus non-chat models.
func fetchOpenAIModels(ctx context.Context) ([]modelEntry, error) {
	cred, err := modelsOpenAIAuth(ctx)
	if err != nil {
		return nil, errors.New("no openai credential on this host")
	}
	hdr := map[string]string{"Authorization": "Bearer " + cred.Token}
	base := strings.TrimRight(modelsOpenAIBase(cred.Kind), "/")
	if cred.Kind == "chatgpt" {
		if cred.AccountID != "" {
			hdr["chatgpt-account-id"] = cred.AccountID
		}
		var r struct {
			Models []struct {
				Slug        string `json:"slug"`
				DisplayName string `json:"display_name"`
				Visibility  string `json:"visibility"`
			} `json:"models"`
		}
		if err := modelsGet(ctx, base+"/models?client_version="+modelsClientVersion, hdr, &r); err != nil {
			return nil, err
		}
		var out []modelEntry
		for _, m := range r.Models {
			if m.Visibility != "" && m.Visibility != "list" {
				continue
			}
			out = append(out, modelEntry{m.Slug, m.DisplayName})
		}
		return out, nil
	}
	var r struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := modelsGet(ctx, base+"/models", hdr, &r); err != nil {
		return nil, err
	}
	var out []modelEntry
next:
	for _, d := range r.Data {
		l := strings.ToLower(d.ID)
		for _, bad := range openaiNonChat {
			if strings.Contains(l, bad) {
				continue next
			}
		}
		out = append(out, modelEntry{d.ID, d.ID})
	}
	return out, nil
}

var modelsFetchers = map[string]func(context.Context) ([]modelEntry, error){
	"claudesdk":  fetchAnthropicModels,
	"venice":     fetchVeniceModels,
	"openai":     fetchOpenAIModels,
	"openrouter": fetchOpenRouterModels,
}

// ---- cache + single-flight -------------------------------------------------

type modelsCached struct {
	list modelList
	exp  time.Time
}

type modelsCall struct {
	done chan struct{}
	list modelList
}

var (
	modelsMu       sync.Mutex
	modelsCache    = map[string]modelsCached{}
	modelsInflight = map[string]*modelsCall{}
	modelsNow      = time.Now
	modelsGen      uint64 // bumped by modelsReset; stale fetches do not write back
)

// modelsFallbackList is the built-in answer with the reason it was used.
func modelsFallbackList(provider, reason string) modelList {
	return modelList{
		Provider: provider,
		Source:   modelsSourceFallback,
		Error:    reason,
		Models:   sanitizeModels(modelsFallback[provider]),
	}
}

// fetchModelList does one uncached fetch for provider, falling back on any
// error or an empty result. It never returns an error itself.
func fetchModelList(provider string) modelList {
	f, ok := modelsFetchers[provider]
	if !ok {
		return modelList{Provider: provider, Source: modelsSourceFallback,
			Error: "unknown provider", Models: []modelEntry{}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsTimeout)
	defer cancel()
	raw, err := f(ctx)
	if err != nil {
		return modelsFallbackList(provider, err.Error())
	}
	clean := sanitizeModels(raw)
	if len(clean) == 0 {
		return modelsFallbackList(provider, "upstream listed no usable models")
	}
	return modelList{Provider: provider, Source: modelsSourceLive, Models: clean}
}

// modelsFor returns the (possibly cached) list for provider. Concurrent
// callers for the same provider share one upstream fetch. The caller's ctx
// only bounds its own wait; the fetch itself runs on its own timeout so one
// impatient caller does not cancel it for the others.
func modelsFor(ctx context.Context, provider string) (modelList, error) {
	if !validProvider(provider) {
		return modelList{}, fmt.Errorf("unknown provider %q", provider)
	}
	modelsMu.Lock()
	if c, ok := modelsCache[provider]; ok && modelsNow().Before(c.exp) {
		modelsMu.Unlock()
		return c.list, nil
	}
	call, ok := modelsInflight[provider]
	if !ok {
		call = &modelsCall{done: make(chan struct{})}
		modelsInflight[provider] = call
		gen := modelsGen
		go func() {
			l := fetchModelList(provider)
			ttl := modelsTTL
			if l.Source != modelsSourceLive {
				ttl = modelsFallbackTTL
			}
			modelsMu.Lock()
			call.list = l
			if gen == modelsGen {
				modelsCache[provider] = modelsCached{list: l, exp: modelsNow().Add(ttl)}
				delete(modelsInflight, provider)
			}
			modelsMu.Unlock()
			close(call.done)
		}()
	}
	modelsMu.Unlock()
	select {
	case <-call.done:
		return call.list, nil
	case <-ctx.Done():
		return modelsFallbackList(provider, "still loading"), nil
	}
}

// modelsReset waits out in-flight fetches (each bounded by modelsTimeout),
// then clears the cache (tests).
func modelsReset() {
	modelsMu.Lock()
	var pending []*modelsCall
	for _, c := range modelsInflight {
		pending = append(pending, c)
	}
	modelsMu.Unlock()
	for _, c := range pending {
		<-c.done
	}
	modelsMu.Lock()
	modelsCache = map[string]modelsCached{}
	modelsInflight = map[string]*modelsCall{}
	modelsGen++
	modelsMu.Unlock()
}

// modelsRPCWait bounds how long the Models RPC waits for a fetch: a little
// over modelsTimeout so a fetch that finishes inside its own budget is
// served live, but the dialog never hangs past it (var for tests).
var modelsRPCWait = modelsTimeout + time.Second

// fetchOpenRouterModels: GET https://openrouter.ai/api/v1/models (OpenAI list
// shape plus supported_parameters and architecture). The catalog has hundreds
// of models; only text-output models that accept "tools" can drive the
// venice_stream.js agent loop, so the rest are dropped.
func fetchOpenRouterModels(ctx context.Context) ([]modelEntry, error) {
	key, err := modelsOpenRouterAuth()
	if err != nil || key == "" {
		return nil, errors.New("no openrouter credential on this host")
	}
	var r struct {
		Data []struct {
			ID                  string   `json:"id"`
			Name                string   `json:"name"`
			SupportedParameters []string `json:"supported_parameters"`
			Architecture        struct {
				OutputModalities []string `json:"output_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	u := strings.TrimRight(modelsOpenRouterBase(), "/") + "/api/v1/models"
	if err := modelsGet(ctx, u, map[string]string{"Authorization": "Bearer " + key}, &r); err != nil {
		return nil, err
	}
	var out []modelEntry
	for _, d := range r.Data {
		if !slices.Contains(d.SupportedParameters, "tools") {
			continue
		}
		if m := d.Architecture.OutputModalities; len(m) > 0 && !slices.Contains(m, "text") {
			continue
		}
		out = append(out, modelEntry{d.ID, d.Name})
	}
	return out, nil
}
