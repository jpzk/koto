package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"koto-protocol/pb"
)

// provEnv points every credential lookup at an empty temp state dir and
// restores the globals afterwards. Nothing is configured until a test
// writes something.
func provEnv(t *testing.T) string {
	t.Helper()
	oh, oc, ok, oe := HERE, credPath, apiKeyPath, envAPIKey
	t.Cleanup(func() { HERE, credPath, apiKeyPath, envAPIKey = oh, oc, ok, oe })
	dir := t.TempDir()
	HERE = dir
	credPath = filepath.Join(dir, ".claude", ".credentials.json")
	apiKeyPath = filepath.Join(dir, "creds", "anthropic-api-key")
	envAPIKey = ""
	t.Setenv("KOTO_UPSTREAM", "")
	for _, d := range []string{filepath.Dir(credPath), filepath.Dir(apiKeyPath)} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func provWrite(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func provGot(t *testing.T) map[string]providerStatus {
	t.Helper()
	m := map[string]providerStatus{}
	for _, s := range providerStatuses() {
		m[s.Name] = s
	}
	if len(m) != len(knownProviders) {
		t.Fatalf("statuses = %+v, want one per known provider", m)
	}
	return m
}

func provWant(t *testing.T, p string, configured bool, kind string) {
	t.Helper()
	s := provGot(t)[p]
	if s.Configured != configured || s.Kind != kind {
		t.Fatalf("%s = %+v, want configured=%v kind=%q", p, s, configured, kind)
	}
}

func TestProviderStatusNothingConfigured(t *testing.T) {
	provEnv(t)
	got := providerStatuses()
	for i, s := range got {
		if s.Name != knownProviders[i] || s.Configured || s.Kind != "" {
			t.Fatalf("status[%d] = %+v, want %s unconfigured", i, s, knownProviders[i])
		}
	}
}

func TestProviderStatusClaude(t *testing.T) {
	t.Run("oauth", func(t *testing.T) {
		provEnv(t)
		provWrite(t, credPath, `{"claudeAiOauth":{"accessToken":"sk-ant-oat-x","expiresAt":1}}`)
		provWant(t, providerClaude, true, credKindSubscription)
	})
	t.Run("oauth file without token", func(t *testing.T) {
		provEnv(t)
		provWrite(t, credPath, `{"claudeAiOauth":{}}`)
		provWant(t, providerClaude, false, "")
	})
	t.Run("oauth file garbage", func(t *testing.T) {
		provEnv(t)
		provWrite(t, credPath, `not json`)
		provWant(t, providerClaude, false, "")
	})
	t.Run("key file", func(t *testing.T) {
		provEnv(t)
		provWrite(t, apiKeyPath, "sk-ant-api-x\n")
		provWant(t, providerClaude, true, credKindAPIKey)
	})
	t.Run("blank key file", func(t *testing.T) {
		provEnv(t)
		provWrite(t, apiKeyPath, " \n")
		provWant(t, providerClaude, false, "")
	})
	t.Run("env key", func(t *testing.T) {
		provEnv(t)
		envAPIKey = "sk-ant-api-env"
		provWant(t, providerClaude, true, credKindEnv)
	})
	t.Run("proxied placeholder alone", func(t *testing.T) {
		provEnv(t)
		envAPIKey = anthropicPlaceholderKey
		provWant(t, providerClaude, false, "")
	})
	t.Run("proxied placeholder shadows files like the proxy", func(t *testing.T) {
		provEnv(t)
		envAPIKey = anthropicPlaceholderKey
		provWrite(t, credPath, `{"claudeAiOauth":{"accessToken":"sk-ant-oat-x","expiresAt":1}}`)
		provWrite(t, apiKeyPath, "sk-ant-api-x")
		// currentAPIKey (what authHeaders sends) is the placeholder, so the
		// detector must not claim the files are in use.
		if currentAPIKey() != anthropicPlaceholderKey {
			t.Fatalf("precondition: proxy would send %q", currentAPIKey())
		}
		provWant(t, providerClaude, false, "")
	})
	t.Run("KOTO_UPSTREAM with placeholder", func(t *testing.T) {
		provEnv(t)
		envAPIKey = anthropicPlaceholderKey
		t.Setenv("KOTO_UPSTREAM", "http://127.0.0.1:18888")
		provWant(t, providerClaude, true, credKindUpstream)
	})
	t.Run("KOTO_UPSTREAM alone", func(t *testing.T) {
		provEnv(t)
		t.Setenv("KOTO_UPSTREAM", "http://127.0.0.1:18888")
		provWant(t, providerClaude, true, credKindUpstream)
		// Upstream fronts Anthropic and OpenAI; venice stays unconfigured.
		provWant(t, providerVenice, false, "")
		provWant(t, providerOpenAI, true, credKindUpstream)
	})
	t.Run("KOTO_UPSTREAM blank", func(t *testing.T) {
		provEnv(t)
		t.Setenv("KOTO_UPSTREAM", "  ")
		provWant(t, providerClaude, false, "")
	})
}

func TestProviderStatusVenice(t *testing.T) {
	provEnv(t)
	provWant(t, providerVenice, false, "")
	provWrite(t, veniceKeyPath(), "\n")
	provWant(t, providerVenice, false, "")
	provWrite(t, veniceKeyPath(), "vk-x\n")
	provWant(t, providerVenice, true, credKindAPIKey)
	provWant(t, providerClaude, false, "")
}

func TestProviderStatusOpenAI(t *testing.T) {
	t.Run("subscription", func(t *testing.T) {
		provEnv(t)
		provWrite(t, openaiAuthPath(), `{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":"at-x","refresh_token":"rt-x"}}`)
		provWant(t, providerOpenAI, true, credKindSubscription)
	})
	t.Run("api key in auth file", func(t *testing.T) {
		provEnv(t)
		provWrite(t, openaiAuthPath(), `{"OPENAI_API_KEY":"sk-proj-x"}`)
		provWant(t, providerOpenAI, true, credKindAPIKey)
	})
	t.Run("api key file", func(t *testing.T) {
		provEnv(t)
		provWrite(t, openaiAPIKeyPath(), "sk-proj-x\n")
		provWant(t, providerOpenAI, true, credKindAPIKey)
	})
	t.Run("empty auth file and blank key", func(t *testing.T) {
		provEnv(t)
		provWrite(t, openaiAuthPath(), `{"OPENAI_API_KEY":"  ","tokens":{"access_token":""}}`)
		provWrite(t, openaiAPIKeyPath(), "\n")
		provWant(t, providerOpenAI, false, "")
	})
	t.Run("garbage auth file", func(t *testing.T) {
		provEnv(t)
		provWrite(t, openaiAuthPath(), `{{{`)
		provWant(t, providerOpenAI, false, "")
	})
}

// The detector and the proxy share openaiStoredCred: whatever kind the
// detector reports is the kind openaiCredential injects.
func TestProviderStatusOpenAIAgreesWithProxy(t *testing.T) {
	provEnv(t)
	provWrite(t, openaiAPIKeyPath(), "sk-proj-agree\n")
	c, err := openaiCredential(context.Background())
	if err != nil || c.Kind != "api-key" || c.Token != "sk-proj-agree" {
		t.Fatalf("proxy cred = %+v, %v", c, err)
	}
	provWant(t, providerOpenAI, true, credKindAPIKey)
}

// The RPC answer carries name/configured/kind and nothing else: seed every
// credential source with a recognisable secret and check none of its bytes
// (nor a prefix, nor the paths) appear anywhere in the marshaled response.
func TestProvidersRPCCarriesNoSecret(t *testing.T) {
	dir := provEnv(t)
	const (
		claudeTok = "sk-ant-oat01-SECRETCLAUDETOKEN0001"
		claudeKey = "sk-ant-api03-SECRETCLAUDEKEY0002"
		veniceKey = "vk-SECRETVENICEKEY0003"
		openaiTok = "eyJSECRETOPENAITOKEN0004"
		openaiRT  = "rt-SECRETOPENAIREFRESH0005"
		openaiKey = "sk-proj-SECRETOPENAIKEY0006"
	)
	provWrite(t, credPath, `{"claudeAiOauth":{"accessToken":"`+claudeTok+`","expiresAt":4102444800000}}`)
	provWrite(t, apiKeyPath, claudeKey)
	provWrite(t, veniceKeyPath(), veniceKey)
	provWrite(t, openaiAuthPath(), `{"tokens":{"access_token":"`+openaiTok+`","refresh_token":"`+openaiRT+`"}}`)
	provWrite(t, openaiAPIKeyPath(), openaiKey)

	resp, err := (&kotoServer{}).Providers(context.Background(), &pb.ProvidersReq{})
	if err != nil || !resp.Ok {
		t.Fatalf("Providers = %+v, %v", resp, err)
	}
	if len(resp.Providers) != len(knownProviders) {
		t.Fatalf("providers = %+v", resp.Providers)
	}
	for i, p := range resp.Providers {
		if p.Name != knownProviders[i] || !p.Configured || p.Kind == "" {
			t.Fatalf("provider[%d] = %+v", i, p)
		}
	}
	wire, err := proto.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	js, err := protojson.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	for _, blob := range [][]byte{wire, js} {
		s := string(blob)
		for _, secret := range []string{claudeTok, claudeKey, veniceKey, openaiTok, openaiRT, openaiKey} {
			// The whole secret and its distinctive core (any 8-byte window
			// would do; the SECRET… marker is unique to the seeds).
			if strings.Contains(s, secret) || strings.Contains(s, secret[:8]) {
				t.Fatalf("response leaks %q: %q", secret, s)
			}
		}
		if strings.Contains(s, "SECRET") || strings.Contains(s, dir) || strings.Contains(s, "creds") {
			t.Fatalf("response leaks credential material or a path: %q", s)
		}
	}
	// And the only populated fields are the documented ones.
	for _, p := range resp.Providers {
		n := 0
		p.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, _ protoreflect.Value) bool {
			switch fd.Name() {
			case "name", "configured", "kind":
			default:
				t.Errorf("unexpected field %s", fd.Name())
			}
			n++
			return true
		})
		if n == 0 {
			t.Fatal("empty provider status")
		}
	}
}

// providers is untargeted and, like models, denied by default to non-admin
// roles until acl.json grants it.
func TestProvidersVerbACL(t *testing.T) {
	if v := verbFromMethod(pb.Koto_Providers_FullMethodName); v != "providers" {
		t.Fatalf("verb = %q", v)
	}
	acl := parseACL([]byte(`{"agent": {"list": "*"}, "picker": {"providers": "*"}}`))
	if roleAllowed(acl, "agent", "providers", "", false) {
		t.Fatal("providers allowed without a grant")
	}
	if !roleAllowed(acl, "picker", "providers", "", false) {
		t.Fatal("granted providers denied")
	}
}
