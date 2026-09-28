// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// MaxModelListBytes bounds a model listing.
//
// A listing is a convenience: the models a person can choose from. An endpoint
// that answers with something enormous is not a listing, and reading an
// unbounded body into memory to find out is not worth the memory.
const MaxModelListBytes = 8 << 20

// ErrModelList reports a model listing that could not be read.
var ErrModelList = errors.New("gateway: model listing failed")

// Model is one model an endpoint offers.
type Model struct {
	// ID is what goes in a request. It is the field every one of the three
	// formats uses, whichever spelling the listing arrived in.
	ID string `json:"id"`
	// DisplayName is what the listing called it, when it called it something
	// other than the identifier. Anthropic does, and a person picking from a list
	// reads that rather than a slug.
	DisplayName string `json:"display_name,omitempty"`
}

// ErrNoModelListEndpoint reports a format with nowhere to ask for its models.
var ErrNoModelListEndpoint = errors.New("gateway: this format has no model listing")

// ListModels asks the endpoint which models it offers.
//
// It goes through the same endpoint resolution and the same headers as a chat
// call, deliberately. A second place that builds the URL and the credentials
// would be a second place they could disagree, and the failure would look like
// "the provider has no models" rather than "two pieces of code spelled the
// address differently".
func (g *ProviderGateway) ListModels(ctx context.Context) ([]Model, error) {
	if g == nil {
		return nil, fmt.Errorf("%w: nil gateway", ErrModelList)
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: context must not be nil", ErrModelList)
	}
	endpoint, err := g.modelListEndpoint()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: build the request: %w", ErrModelList, err)
	}
	request.Header.Set("Accept", "application/json")
	for name, value := range g.requestHeaders() {
		request.Header.Set(name, value)
	}
	response, err := g.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrModelList, g.adapter.Format(), err)
	}
	defer func() {
		_ = response.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(response.Body, MaxModelListBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: read the response: %w", ErrModelList, err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: %s returned HTTP %d: %s",
			ErrModelList, g.adapter.Format(), response.StatusCode, strings.TrimSpace(string(body)))
	}
	models, err := parseModelList(body)
	if err != nil {
		return nil, err
	}
	return models, nil
}

// modelListEndpoint is the address the listing is at.
//
// It is the same base the chat call is built from, with the format's own
// conversation path replaced by the listing path. Deriving it that way rather
// than by trimming the conversation path is what makes it right for an endpoint
// whose base already carries a version: a base of https://host/v1 becomes
// https://host/v1/models, and trimming would have produced https://host/models.
func (g *ProviderGateway) modelListEndpoint() (string, error) {
	conversations, err := resolveEndpoint(g.config)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrModelList, err)
	}
	base, _, _ := strings.Cut(conversations, "/chat/completions")
	base, _, _ = strings.Cut(base, "/messages")
	base, _, _ = strings.Cut(base, "/responses")
	if base == "" {
		return "", fmt.Errorf("%w: %v", ErrNoModelListEndpoint, g.config.Endpoint)
	}
	return base + "/models", nil
}

// parseModelList reads a listing in whichever of the shapes the endpoints use.
//
// The shape is not one thing. OpenAI's own and every gateway speaking its
// format answer {"data":[{"id":…}]}; Ollama's own API answers
// {"models":[{"name":…}]}; Anthropic answers the OpenAI shape with a display
// name beside the identifier. All three are read, because refusing a listing
// because it used a different field name would be refusing the local endpoints
// this project exists to support.
func parseModelList(body []byte) ([]Model, error) {
	var listing struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Name        string `json:"name"`
			Model       string `json:"model"`
		} `json:"data"`
		Models []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Name        string `json:"name"`
			Model       string `json:"model"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &listing); err != nil {
		return nil, fmt.Errorf("%w: the response is not a model listing: %w", ErrModelList, err)
	}
	models := make([]Model, 0, len(listing.Data)+len(listing.Models))
	seen := make(map[string]struct{}, len(listing.Data)+len(listing.Models))
	appendOne := func(id, display string) {
		identifier := strings.TrimSpace(id)
		if identifier == "" {
			return
		}
		if _, already := seen[identifier]; already {
			// Two entries with one identifier is a listing that lists a model
			// twice, and a picker showing it twice is a picker with a mistake in it.
			return
		}
		seen[identifier] = struct{}{}
		models = append(models, Model{ID: identifier, DisplayName: strings.TrimSpace(display)})
	}
	for _, entry := range listing.Data {
		appendOne(firstNonEmpty(entry.ID, entry.Name, entry.Model), entry.DisplayName)
	}
	for _, entry := range listing.Models {
		appendOne(firstNonEmpty(entry.Name, entry.ID, entry.Model), entry.DisplayName)
	}
	// Sorted, because a listing in whatever order the endpoint chose is not a
	// list a person can find anything in, and re-sorting it here is the only
	// place that has to know.
	sort.Slice(models, func(left, right int) bool { return models[left].ID < models[right].ID })
	return models, nil
}
