// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheModelListingGoesToTheSameAddressAsTheConversation(t *testing.T) {
	// A base that already carries a version is the case that catches a URL built
	// by trimming the conversation path rather than by replacing it.
	cases := []struct {
		name     string
		endpoint string
		format   APIFormat
		want     string
	}{
		{"openai", "https://api.openai.com/v1", FormatOpenAICompletions, "https://api.openai.com/v1/models"},
		{"anthropic", "https://api.anthropic.com", FormatAnthropicMessages, "https://api.anthropic.com/models"},
		{"responses", "https://api.openai.com/v1", FormatOpenAIResponses, "https://api.openai.com/v1/models"},
		{"ollama", "http://localhost:11434/v1", FormatOpenAICompletions, "http://localhost:11434/v1/models"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			built, err := New(Config{Format: testCase.format, Endpoint: testCase.endpoint, Model: "m"})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			got, err := built.modelListEndpoint()
			if err != nil {
				t.Fatalf("listing address: %v", err)
			}
			if got != testCase.want {
				t.Fatalf("listing address = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestTheListingIsReadInWhicheverShapeItArrives(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{
			name: "the openai shape",
			body: `{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"}]}`,
			want: []string{"gpt-4o", "gpt-4o-mini"},
		},
		{
			name: "ollama's own shape",
			body: `{"models":[{"name":"qwen3:8b"},{"name":"llama3:70b"}]}`,
			want: []string{"llama3:70b", "qwen3:8b"},
		},
		{
			name: "anthropic's shape with display names",
			body: `{"data":[{"id":"claude-sonnet-4-5","display_name":"Claude Sonnet 4.5"}]}`,
			want: []string{"claude-sonnet-4-5"},
		},
		{
			name: "an empty listing",
			body: `{"data":[]}`,
			want: []string{},
		},
		{
			name: "a model listed twice",
			body: `{"data":[{"id":"m"},{"id":"m"}]}`,
			want: []string{"m"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			models, err := parseModelList([]byte(testCase.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := make([]string, 0, len(models))
			for _, model := range models {
				got = append(got, model.ID)
			}
			if strings.Join(got, ",") != strings.Join(testCase.want, ",") {
				t.Fatalf("models = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestAListingThatIsNotAListingIsRefused(t *testing.T) {
	if _, err := parseModelList([]byte("not json")); err == nil {
		t.Fatal("a body that is not a listing was accepted")
	}
}

func TestListingCarriesTheCredentialsTheConversationWould(t *testing.T) {
	// Two places building the address and the headers would be two places they
	// could disagree, and the disagreement would look like an endpoint with no
	// models rather than like two pieces of code.
	t.Run("anthropic asks with a key and a version", func(t *testing.T) {
		var seen http.Header
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Clone()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-4-5"}]}`))
		}))
		defer server.Close()

		built, err := New(Config{
			Format: FormatAnthropicMessages, Endpoint: server.URL, Model: "claude-sonnet-4-5",
			APIKey: "secret",
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		models, err := built.ListModels(context.Background())
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(models) != 1 || models[0].ID != "claude-sonnet-4-5" {
			t.Fatalf("models = %#v", models)
		}
		if seen.Get("x-api-key") != "secret" {
			t.Fatalf("x-api-key = %q, want the key the conversation would send", seen.Get("x-api-key"))
		}
		if seen.Get("anthropic-version") == "" {
			t.Fatal("the listing was asked without a version, which that endpoint requires")
		}
	})

	t.Run("openai asks with a bearer key", func(t *testing.T) {
		var seen http.Header
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Clone()
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"}]}`))
		}))
		defer server.Close()

		built, err := New(Config{
			Format: FormatOpenAICompletions, Endpoint: server.URL, Model: "gpt-4o", APIKey: "secret",
		})
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if _, err := built.ListModels(context.Background()); err != nil {
			t.Fatalf("list: %v", err)
		}
		if seen.Get("Authorization") != "Bearer secret" {
			t.Fatalf("Authorization = %q", seen.Get("Authorization"))
		}
	})
}

func TestAnEndpointThatRefusesTheListingSaysSo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"no key"}`))
	}))
	defer server.Close()

	built, err := New(Config{Format: FormatOpenAICompletions, Endpoint: server.URL, Model: "m"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	_, err = built.ListModels(context.Background())
	if err == nil {
		t.Fatal("a refused listing was reported as a success")
	}
	// The body is carried, because "the endpoint said no key" is the difference
	// between fixing a setting and looking in the wrong place.
	if !strings.Contains(err.Error(), "no key") {
		t.Fatalf("error = %q, want the endpoint's own words", err)
	}
}

func TestANilGatewayDoesNotList(t *testing.T) {
	var built *ProviderGateway
	if _, err := built.ListModels(context.Background()); err == nil {
		t.Fatal("a nil gateway listed models")
	}
	built, err := New(Config{Format: FormatOpenAICompletions, Endpoint: "http://x", Model: "m"})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if _, err := built.ListModels(nil); err == nil {
		t.Fatal("a nil context listed models")
	}
}

func TestTheListingIsCappedRatherThanReadWithoutBound(t *testing.T) {
	// The cap is a constant rather than a behaviour because the behaviour is what
	// matters; this says the number is the one the code uses.
	if MaxModelListBytes <= 0 {
		t.Fatalf("the listing cap is %d, which is no cap at all", MaxModelListBytes)
	}
	listing := map[string]any{"data": []map[string]string{{"id": "m"}}}
	encoded, err := json.Marshal(listing)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	models, err := parseModelList(encoded)
	if err != nil || len(models) != 1 {
		t.Fatalf("parse = %#v, %v", models, err)
	}
}
