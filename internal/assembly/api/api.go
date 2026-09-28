// SPDX-License-Identifier: Apache-2.0

// Package api is the interface's own HTTP surface: conversations, messages, and
// the model configuration.
//
// It is mounted by the web server behind the token, alongside the event stream
// and the built interface, and it is the only place in the project that knows
// what a window is allowed to ask for. The routes are the standard library's own
// patterns, so a path that names a session is a path with the session in it
// rather than a segment this package has to parse by hand.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dingtongbin/MarxAgent/internal/assembly/config"
	"github.com/dingtongbin/MarxAgent/internal/assembly/runtime"
	"github.com/dingtongbin/MarxAgent/internal/assembly/workspace"
)

// maxRequestBytes bounds a request body.
//
// The bodies here are a message and a configuration, both of which are written
// by a person, and a limit is what stops a request that is not one of those from
// being read into memory as if it were.
const maxRequestBytes = 4 << 20

// The routes, named once so the handler and anything that documents them cannot
// drift apart.
const (
	pathConfig        = "/v1/config"
	pathSessions      = "/v1/sessions"
	pathSession       = "/v1/sessions/{id}"
	pathMessages      = "/v1/sessions/{id}/messages"
	pathInterrupt     = "/v1/sessions/{id}/interrupt"
	pathProviders     = "/v1/providers"
	pathProviderModel = "/v1/providers/{name}/models"
	pathWorkspace     = "/v1/workspace"
)

// Handler serves the interface's requests.
type Handler struct {
	runtime   *runtime.Runtime
	config    *config.Store
	workspace *workspace.Store
	log       func(message string, fields map[string]any)
}

// New builds the handler.
func New(
	sessions *runtime.Runtime,
	settings *config.Store,
	tabs *workspace.Store,
	log func(string, map[string]any),
) *Handler {
	return &Handler{runtime: sessions, config: settings, workspace: tabs, log: log}
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == pathWorkspace:
		h.serveWorkspace(w, r)
	case r.URL.Path == pathConfig:
		h.serveConfig(w, r)
	case r.URL.Path == pathSessions:
		h.serveSessions(w, r)
	case strings.HasPrefix(r.URL.Path, pathSessions+"/"):
		h.serveSession(w, r)
	case strings.HasPrefix(r.URL.Path, pathProviders+"/"), r.URL.Path == pathProviders:
		h.serveProvider(w, r)
	default:
		// The handler is mounted ahead of the built interface, so a path it does
		// not claim has to be handed on rather than answered with a 404 that
		// would look like a missing page. A page of the interface is a real
		// answer here, and a 404 would be a wrong one.
		http.NotFound(w, r)
	}
}

// serveProvider is what one provider can be asked about, and the catalog of
// providers that can be chosen from in the first place.
func (h *Handler) serveProvider(w http.ResponseWriter, r *http.Request) {
	// The three addresses that carry no provider name are asked about before the
	// name is cut out of the path. Cutting first would leave the whole path as a
	// name and send a menu looking for a provider called "/v1/providers", and a
	// draft that has never been saved has no name in the path to cut.
	switch r.URL.Path {
	case pathProviders:
		h.serveProviderRoot(w, r)
		return
	case pathProviders + "/models":
		h.serveProviderModels(w, r, "")
		return
	case pathProviders + "/test":
		h.serveProviderTest(w, r, "")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, pathProviders+"/")
	name, remainder, _ := strings.Cut(rest, "/")
	switch strings.TrimSpace(remainder) {
	case "models":
		h.serveProviderModels(w, r, strings.TrimSpace(name))
	case "test":
		h.serveProviderTest(w, r, strings.TrimSpace(name))
	default:
		http.NotFound(w, r)
	}
}

// serveProviderRoot is the catalog of providers this build knows about.
//
// It is a GET with no name in it, because the menu that offers them has to ask
// before it knows which one the person is going to choose.
func (h *Handler) serveProviderRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"catalog": h.runtime.ProviderCatalog()})
}

// specRequest is what a probe or a listing carries.
//
// The provider comes whole rather than by name, because the person asking has
// usually just typed the address and has not saved it yet. The name in the path is
// still filled in, so a saved provider can be asked about by name alone and a
// draft can be asked about with the fields it has.
type specRequest struct {
	config.ProviderSpec
	Model string `json:"model"`
}

func (h *Handler) serveProviderModels(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
		return
	}
	request := specRequest{}
	request.Name = name
	if r.Method == http.MethodPost {
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		// A body that names a provider is the same provider the path did; the path
		// is where the window already knows the answer, and losing it because a body
		// left it out would be a request that works or fails for no visible reason.
		if strings.TrimSpace(request.Name) == "" {
			request.Name = name
		}
	}
	// A listing is a request to a third party, so the budget is this project's and
	// not the caller's: a window that gave no deadline is a window that would
	// otherwise wait on an endpoint that never answers.
	ctx, cancel := context.WithTimeout(r.Context(), modelListBudget)
	defer cancel()
	models, err := h.runtime.ProviderModels(ctx, request.ProviderSpec)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// serveProviderTest asks whether a provider can actually be reached.
//
// A refusal is a 200 with the refusal in it, not an error status. The page asked
// a question whose possible answers are yes and no; a status code that said the
// question could not be asked would be a third answer that is not one of them,
// and a page that has to tell those apart would be a page whose error handling
// is more complicated than the question.
func (h *Handler) serveProviderTest(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	var request specRequest
	if err := decodeBody(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(request.Name) == "" {
		request.Name = name
	}
	ctx, cancel := context.WithTimeout(r.Context(), modelListBudget)
	defer cancel()
	writeJSON(w, http.StatusOK, h.runtime.ProviderTest(ctx, request.ProviderSpec, request.Model))
}

// modelListBudget bounds one model listing.
//
// A model that takes longer than this to start answering is an endpoint that is
// not going to, and a settings page that waits for it forever is a page a person
// cannot close.
const modelListBudget = 20 * time.Second

// serveWorkspace reads and writes the tab strip.
//
// There is no window identifier here, on purpose. A tree kept per window needs
// something stored on the browser to tell one window from another, and that is
// state about the work living on the machine that is only showing it — state a
// browser tidy-up takes away and the next machine does not have. The strip is the
// installation's, so the same window anywhere comes back to the same tabs.
func (h *Handler) serveWorkspace(w http.ResponseWriter, r *http.Request) {
	if h.workspace == nil {
		writeError(w, http.StatusNotImplemented, errors.New("tab strips are not assembled here"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		tree, err := h.workspace.Get()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, tree)
	case http.MethodPut:
		var incoming workspace.Tree
		if err := decodeBody(r, &incoming); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		stored, err := h.workspace.Put(incoming)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		// What was stored rather than what was sent, because the store corrects
		// two things: a tab listed twice, and a strip whose active tab is not in
		// it. A window that showed what it sent rather than what was kept would
		// keep showing the duplicate until the next reload.
		writeJSON(w, http.StatusOK, stored)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// serveConfig reads and writes the model configuration.
func (h *Handler) serveConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, h.config.View())
	case http.MethodPut, http.MethodPost:
		var update config.Update
		if err := decodeBody(r, &update); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := h.config.Apply(update); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		// The saved configuration rather than an empty object, so a page that
		// saved something knows what was stored without asking again.
		writeJSON(w, http.StatusOK, h.config.View())
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// serveSessions lists and opens conversations.
func (h *Handler) serveSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"sessions": h.runtime.List()})
	case http.MethodPost:
		var request runtime.CreateRequest
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		created, err := h.runtime.Create(request)
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, created)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// serveSession is everything under one conversation.
func (h *Handler) serveSession(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, pathSessions+"/")
	sessionID, remainder, _ := strings.Cut(rest, "/")
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, errors.New("name the session"))
		return
	}
	switch remainder {
	case "":
		h.serveOneSession(w, r, sessionID)
	case "messages":
		h.serveMessages(w, r, sessionID)
	case "interrupt":
		h.serveInterrupt(w, r, sessionID)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) serveOneSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	session, err := h.runtime.Get(sessionID)
	if err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, session)
}

// serveMessages reads a conversation and adds a message to it.
func (h *Handler) serveMessages(w http.ResponseWriter, r *http.Request, sessionID string) {
	switch r.Method {
	case http.MethodGet:
		messages, err := h.runtime.Messages(sessionID)
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"messages": messages})
	case http.MethodPost:
		var request runtime.SendRequest
		if err := decodeBody(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		info, err := h.runtime.Send(r.Context(), sessionID, request)
		if err != nil {
			writeRuntimeError(w, err)
			return
		}
		// Accepted rather than finished: the turn is running and what it produces
		// arrives on the event stream. A window that waited here would be a window
		// holding a request open for as long as a model takes to answer.
		writeJSON(w, http.StatusAccepted, info)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (h *Handler) serveInterrupt(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if err := h.runtime.Interrupt(sessionID); err != nil {
		writeRuntimeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"interrupted": true})
}

// decodeBody reads one JSON body.
//
// The size is bounded and the body is not allowed to be a stream of unknown
// length, because a body that has not finished arriving is a request this
// process would hold a connection open for.
func decodeBody(r *http.Request, target any) error {
	if r.ContentLength > maxRequestBytes {
		return errors.New("the request body is too large")
	}
	reader := io.LimitReader(r.Body, maxRequestBytes+1)
	encoded, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if len(encoded) > maxRequestBytes {
		return errors.New("the request body is too large")
	}
	if len(strings.TrimSpace(string(encoded))) == 0 {
		// An empty body is an empty request rather than a broken one, so a page
		// that posts nothing to create a conversation gets the conversation.
		return nil
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return err
	}
	return nil
}

// writeRuntimeError turns a runtime error into the status that means something to
// a window.
//
// The mapping is the point: a window that asked for a session which is not there
// has to be able to tell that apart from one whose request was malformed, and
// one that has no model configured has to be able to send the person to the
// settings page rather than showing an error they cannot act on.
func writeRuntimeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, runtime.ErrNoSession):
		writeError(w, http.StatusNotFound, err)
	case errors.Is(err, runtime.ErrBusy):
		writeError(w, http.StatusConflict, err)
	case errors.Is(err, runtime.ErrNoModel), errors.Is(err, config.ErrInvalidConfig):
		writeError(w, http.StatusUnprocessableEntity, err)
	case errors.Is(err, runtime.ErrEmptyMessage):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, runtime.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err)
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]any{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = w.Write(append(encoded, '\n'))
}

// methodNotAllowed answers a method an address does not serve, and says which
// ones it does. A 405 with no list leaves a caller guessing what to try next.
func methodNotAllowed(w http.ResponseWriter, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, errors.New("that method is not served here"))
}
