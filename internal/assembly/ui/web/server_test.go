// SPDX-License-Identifier: Apache-2.0

package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A health check that needs a secret is a check nobody can run. This one is the only
// address served without a token, and it was once behind the check anyway while the
// comment above it said it was not — which is the exact shape of a bug a test is for.
func TestTheHealthCheckIsAnsweredWithoutTheToken(t *testing.T) {
	server, _ := newTestServer(t, nil)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, healthPath, nil)
	server.handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("the health check answered %d without a token", recorder.Code)
	}
	if got := recorder.Body.String(); strings.TrimSpace(got) != "ok" {
		t.Fatalf("the health check said %q", got)
	}
	// Nothing about the process is disclosed, because this address is open. A
	// version or a path here would be free information for anything on the machine.
	if strings.Contains(recorder.Body.String(), "token") {
		t.Fatal("the health check mentioned the token")
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("the health check is cacheable: %q", cache)
	}
	// A method that is not served at this address is not answered by it. A POST falls
	// through to the route that catches what was not claimed, which is behind the
	// token, so a prober posting here is told nothing at all about the process.
	recorder = httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, healthPath, nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("a POST to the health check answered %d", recorder.Code)
	}
	// And the exemption is only for that one path. A path that merely starts with the
	// same letters is a different path and still needs the token.
	recorder = httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, healthPath+"x", nil))
	if recorder.Code == http.StatusOK {
		t.Fatal("a path that merely resembles the health check was let past")
	}
}

// Everything that is not the health check needs the token. A server that serves its
// own interface to anything on the machine that asks is a server that hands an agent
// to whatever is running.
func TestNothingElseIsServedWithoutTheToken(t *testing.T) {
	server, _ := newTestServer(t, nil)
	paths := []string{
		"/",
		"/events?session=a",
		"/assets/index-abc123.js",
		"/anything",
	}
	for _, path := range paths {
		recorder := httptest.NewRecorder()
		server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s was served with no token: %d", path, recorder.Code)
		}
	}
}

// The token arrives in whichever form the caller can manage, because a browser cannot
// set a header on a page request and a socket cannot set a header at all.
func TestTheTokenIsTakenFromWhereverItArrives(t *testing.T) {
	server, _ := newTestServer(t, nil)
	cases := []struct {
		name  string
		build func(token string) *http.Request
	}{
		{"a header", func(token string) *http.Request {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", "Bearer "+token)
			return request
		}},
		{"a cookie", func(token string) *http.Request {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.AddCookie(&http.Cookie{Name: tokenCookie, Value: token})
			return request
		}},
		{"a query", func(token string) *http.Request {
			return httptest.NewRequest(http.MethodGet, "/?token="+token, nil)
		}},
	}
	for _, testCase := range cases {
		recorder := httptest.NewRecorder()
		server.handler.ServeHTTP(recorder, testCase.build(server.Token()))
		if recorder.Code != http.StatusOK {
			t.Fatalf("the token from %s was refused: %d", testCase.name, recorder.Code)
		}
	}
}

// A token that is wrong, empty or malformed gets one answer, because three different
// answers would tell a caller which of the three to fix.
func TestAWrongTokenIsRefusedTheSameWayAsNoToken(t *testing.T) {
	server, _ := newTestServer(t, nil)
	var bodies []string
	attempts := []func() *http.Request{
		func() *http.Request { return httptest.NewRequest(http.MethodGet, "/", nil) },
		func() *http.Request {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", "Bearer wrong")
			return request
		},
		func() *http.Request {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", "Bearer "+server.Token()+"x")
			return request
		},
		func() *http.Request {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.Header.Set("Authorization", server.Token())
			return request
		},
	}
	for _, attempt := range attempts {
		recorder := httptest.NewRecorder()
		server.handler.ServeHTTP(recorder, attempt())
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("a bad token was served: %d", recorder.Code)
		}
		bodies = append(bodies, recorder.Body.String())
	}
	for _, body := range bodies[1:] {
		if body != bodies[0] {
			t.Fatalf("two bad tokens got different answers:\n%q\n%q", bodies[0], body)
		}
	}
}

// The token is made fresh and is long enough that guessing is not a plan.
func TestTheTokenIsFreshAndLongEnough(t *testing.T) {
	seen := make(map[string]bool)
	for range 20 {
		token, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatal("the same token came up twice")
		}
		seen[token] = true
		// 32 random bytes are 43 characters once they are written in a URL-safe way.
		if len(token) < 40 {
			t.Fatalf("a token is %d characters long", len(token))
		}
		// A token goes in a URL and in a cookie, so the characters that would need
		// escaping in either must not be there.
		if strings.ContainsAny(token, "+/= ") {
			t.Fatalf("a token needs escaping: %q", token)
		}
	}
}

// A server with no listener, no token or no stream is refused at construction rather
// than at the first request, because a caller that has a server in hand believes it
// works.
func TestAServerSaysWhatItIsMissing(t *testing.T) {
	cases := []struct {
		name   string
		config Config
		want   error
	}{
		{"no listener", Config{Token: "t", Stream: &testStream{}}, ErrNoListener},
		{"no token", Config{Listener: testListener(t), Stream: &testStream{}}, ErrNoToken},
		{"a blank token", Config{Listener: testListener(t), Token: "  ", Stream: &testStream{}}, ErrNoToken},
		{"no stream", Config{Listener: testListener(t), Token: "t"}, ErrNoStream},
	}
	for _, testCase := range cases {
		_, err := New(testCase.config)
		if err != testCase.want {
			t.Fatalf("a server with %s reported %v, want %v", testCase.name, err, testCase.want)
		}
	}
}

// The address is loopback, always. A server that listened on every interface would put
// an agent on the network with a token as the only thing between it and anybody.
func TestTheServerListensOnLoopbackOnly(t *testing.T) {
	listener, err := LoopbackListener()
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	host, _, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("the server would listen on %s", host)
	}
	// The port is the host's choice, so a second server does not collide with the
	// first and neither of them has to be told which number to take.
	other, err := LoopbackListener()
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if other.Addr().String() == listener.Addr().String() {
		t.Fatal("two servers took the same port")
	}
}

// The address handed to the person carries the token in the fragment, because a
// fragment is never sent to the server and a query is, and a token in a query is a
// token in every access log on the way here.
func TestTheAddressCarriesTheTokenInTheFragment(t *testing.T) {
	server, _ := newTestServer(t, nil)
	url := server.URL()
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("the address is %q", url)
	}
	// Cut gives the part before the separator first, then the part after.
	_, fragment, found := strings.Cut(url, "#")
	if !found || fragment != "token="+server.Token() {
		t.Fatalf("the token is not in the fragment: %q", url)
	}
	if strings.Contains(url, "?"+server.Token()) {
		t.Fatalf("the token is in the query: %q", url)
	}
}

// A stream is a stream of events with a position, and a window that reconnects is
// told to carry on from the last one it saw. That is the whole reason this is Server
// Sent Events and not a WebSocket.
func TestAReconnectingStreamIsToldWhereToCarryOnFrom(t *testing.T) {
	stream := &testStream{events: []Event{
		{Seq: 1, Payload: json.RawMessage(`{"type":"partial","data":"one"}`)},
		{Seq: 2, Payload: json.RawMessage(`{"type":"partial","data":"two"}`)},
		{Seq: 3, Payload: json.RawMessage(`{"type":"done"}`)},
	}}
	server, _ := newTestServer(t, stream)
	token := server.Token()

	// The first window gets everything.
	first := openStream(t, server, token, "", 0)
	defer first.Close()
	frame := first.await(t)
	if !strings.Contains(frame, "id: 1") {
		t.Fatalf("the first frame is %q", frame)
	}

	// A window that has seen up to 1 gets the rest and not the beginning. This is the
	// case a WebSocket has no way to express.
	second := openStream(t, server, token, "1", 1)
	defer second.Close()
	after := second.await(t)
	if !strings.Contains(after, "id: 2") {
		t.Fatalf("after resuming from 1 the window was sent %q", after)
	}
	if strings.Contains(after, "id: 1") {
		t.Fatalf("a window that had already seen 1 was sent it again: %q", after)
	}
	if server.Resumes() != 1 {
		t.Fatalf("the resume was not counted: %d", server.Resumes())
	}
}

// The frames are what the browser's event stream parser expects, because a frame that
// is nearly right does not fail loudly. It is dropped, and the window simply shows
// nothing.
func TestAFrameIsWrittenTheWayTheParserNeeds(t *testing.T) {
	// A real newline byte in the payload, not the two characters backslash and n. The
	// escaped form is a single line and needs no splitting; the byte is what would end
	// the frame early and have the rest of the event read as fields of it.
	stream := &testStream{events: []Event{
		{Seq: 7, Payload: json.RawMessage("{\"data\":\"a\nb\"}")},
	}}
	server, _ := newTestServer(t, stream)
	defer stream.close()

	frame := openStream(t, server, server.Token(), "", 0).await(t)
	if !strings.HasPrefix(frame, "retry: ") {
		t.Fatalf("the stream did not say how patient to be: %q", frame)
	}
	if !strings.Contains(frame, "id: 7\n") {
		t.Fatalf("the frame has no id, so a reconnect would ask for it again: %q", frame)
	}
	// A payload with a newline in it has to be written as more than one data line,
	// because one line would end the frame early and the rest would be read as fields.
	if !strings.Contains(frame, "data: {\"data\":\"a\n") {
		t.Fatalf("the payload was not split across data lines: %q", frame)
	}
	if !strings.Contains(frame, "data: b\"}") {
		t.Fatalf("the second line of the payload is missing: %q", frame)
	}
	if !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("the frame was not terminated: %q", frame)
	}
}

// A last event id that is not a position is ignored rather than guessed at, because
// guessing means either replaying everything or skipping a gap, and both are worse
// than starting again from a known point.
func TestAMalformedLastEventIDIsIgnoredRatherThanGuessedAt(t *testing.T) {
	handler := newSSEHandler(&testStream{}, make(chan struct{}))
	for name, header := range map[string]string{
		"not a number":    "not-a-number",
		"negative":        "-1",
		"empty":           "",
		"whitespace":      "  ",
		"a decimal":       "1.5",
		"a huge overflow": "99999999999999999999999",
	} {
		request := httptest.NewRequest(http.MethodGet, "/events?session=a", nil)
		if header != "" {
			request.Header.Set("Last-Event-ID", header)
		}
		after, resumed := handler.resumeFrom(request)
		if after != 0 || resumed {
			t.Fatalf("a last event id of %s was taken as %d (resumed %v)", name, after, resumed)
		}
	}
	// And a real position is taken, which is the case the feature exists for.
	request := httptest.NewRequest(http.MethodGet, "/events?session=a", nil)
	request.Header.Set("Last-Event-ID", "42")
	if after, resumed := handler.resumeFrom(request); after != 42 || !resumed {
		t.Fatalf("a last event id of 42 became %d (resumed %v)", after, resumed)
	}
}

// The window count cannot go below zero, however the handlers interleave, because a
// negative count would make the health of the whole server look like a fault.
func TestTheWindowCountDoesNotGoNegative(t *testing.T) {
	handler := newSSEHandler(&testStream{}, make(chan struct{}))
	handler.countWindow(1, false)
	handler.countWindow(-1, false)
	handler.countWindow(-1, false)
	if got := handler.Windows(); got != 0 {
		t.Fatalf("the window count is %d after more closes than opens", got)
	}
	// A resume is counted separately, because a stream that keeps resuming and one
	// that is being watched for the first time are different things and only one of
	// them says the resume mechanism is being used.
	handler.countWindow(1, true)
	handler.countWindow(1, true)
	if got := handler.Resumers(); got != 2 {
		t.Fatalf("%d resumes were counted", got)
	}
}

// A health check is asked with HEAD by things that want to know whether the process is
// up without wanting what it says.
func TestTheHealthCheckAnswersAHeadWithoutABody(t *testing.T) {
	server, _ := newTestServer(t, nil)
	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodHead, healthPath, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("a HEAD to the health check answered %d", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("a HEAD returned a body: %q", recorder.Body.String())
	}
}

// A server told to serve from a directory does so, and one told to serve from the
// binary does that instead. The two are separate answers to separate questions.
func TestTheServerIsToldWhereToTakeTheInterfaceFrom(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	listener := testListener(t)

	fromBinary, err := New(Config{Listener: listener, Token: token, Stream: &testStream{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := fromBinary.assets()
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(recorder.Body.String(), "<div id=\"root\">") {
		t.Fatalf("the binary did not serve the interface: %.100q", recorder.Body.String())
	}

	// The same server, pointed at a directory that is not there, says so rather than
	// quietly falling back to the binary. Falling back would mean a developer with a
	// mistyped path looking at a working interface and wondering why their changes
	// are not appearing.
	dir := t.TempDir()
	listener2 := testListener(t)
	fromDir, err := New(Config{Listener: listener2, Token: token, Stream: &testStream{}, WebDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	handler, err = fromDir.assets()
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("an empty directory answered %d", recorder.Code)
	}
}

// A stream with no session is refused, because a window watching everything cannot be
// reasoned about when it is three windows watching three different sessions.
func TestAStreamMustNameItsSession(t *testing.T) {
	server, _ := newTestServer(t, nil)
	// A session that is only whitespace is no session. It is written encoded,
	// because a raw space in a request line is a malformed request rather than a
	// blank parameter, and this is testing the server's answer rather than the
	// parser's.
	for _, path := range []string{"/events", "/events?session=%20%20%20"} {
		recorder := httptest.NewRecorder()
		server.handler.ServeHTTP(recorder, authorised(httptest.NewRequest(http.MethodGet, path, nil), server.Token()))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s was accepted: %d", path, recorder.Code)
		}
	}
}

// An event is put on the wire and a window watching the same session is told about
// it, because the server is the only source of what happened and every window
// watching has to hear the same thing.
func TestEveryWindowWatchingASessionSeesTheSameEvents(t *testing.T) {
	stream := &testStream{events: []Event{{Seq: 1, Payload: json.RawMessage(`{"type":"done"}`)}}}
	server, _ := newTestServer(t, stream)
	token := server.Token()

	first := openStream(t, server, token, "", 0)
	defer first.Close()
	second := openStream(t, server, token, "", 0)
	defer second.Close()

	// Wait for both to be counted, because a window that is not registered yet has
	// not proved it would receive anything.
	deadline := time.Now().Add(2 * time.Second)
	for server.Watchers() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := server.Watchers(); got != 2 {
		t.Fatalf("%d windows are watching, want two", got)
	}
	for index, window := range []*streamConn{first, second} {
		frame := window.await(t)
		if !strings.Contains(frame, "done") {
			t.Fatalf("window %d saw %q", index, frame)
		}
	}
}

// A window that goes away is not remembered, because a server that keeps every
// session ever opened grows for as long as it runs and helps nobody.
func TestAClosedWindowIsForgotten(t *testing.T) {
	stream := &testStream{events: []Event{{Seq: 1, Payload: json.RawMessage(`{"type":"done"}`)}}}
	server, _ := newTestServer(t, stream)
	window := openStream(t, server, server.Token(), "", 0)
	window.await(t)

	deadline := time.Now().Add(2 * time.Second)
	for server.Watchers() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.Watchers() == 0 {
		t.Fatal("the window was never counted")
	}
	window.Close()

	deadline = time.Now().Add(2 * time.Second)
	for server.Watchers() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := server.Watchers(); got != 0 {
		t.Fatalf("%d windows are still counted after one closed", got)
	}
}

// The interface is served from the binary by default and from a directory when asked,
// and the difference that matters is the cache: a file being edited must not be one
// a browser keeps.
func TestTheInterfaceIsServedFromTheBinaryOrFromDisk(t *testing.T) {
	server, _ := newTestServer(t, nil)
	recorder := httptest.NewRecorder()
	server.handler.ServeHTTP(recorder, authorised(httptest.NewRequest(http.MethodGet, "/", nil), server.Token()))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the interface is not in the binary: %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "<div id=\"root\">") {
		t.Fatalf("what was served is not the interface: %.120q", recorder.Body.String())
	}

	// From a directory, nothing is cached, because everything there is being edited.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<div id=\"root\">dev</div>"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := StaticHandlerFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("a file being edited is cached as %q", cache)
	}
	if !strings.Contains(recorder.Body.String(), "dev") {
		t.Fatalf("the directory was not served: %.120q", recorder.Body.String())
	}
}

// A directory that is not there is said plainly, because a developer who mistyped
// a path would otherwise be looking at a blank page wondering why.
func TestADirectoryThatIsNotThereIsSaid(t *testing.T) {
	dir := t.TempDir()
	if _, err := StaticHandlerFromDir(filepath.Join(dir, "no-such-dir")); err == nil {
		t.Fatal("a directory that is not there was accepted")
	}
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := StaticHandlerFromDir(file); err == nil {
		t.Fatal("a file was accepted as a directory")
	}
}

// A built asset is fingerprinted, so a browser may keep it for as long as it exists.
// The entry document is not, because its name never changes and it is what points at
// the new ones.
//
// The two serving paths are checked separately because they deliberately differ. From
// the binary, a fingerprinted asset is cacheable. From a directory, nothing is,
// because everything there is being edited and a browser holding yesterday's file is
// the most confusing thing a developer can be shown.
func TestBuiltAssetsAreCacheableAndTheDocumentIsNot(t *testing.T) {
	// The decision about a path is the same wherever the files come from.
	for path, want := range map[string]bool{
		"/assets/index-abc123.js":  true,
		"/assets/index-abc123.css": true,
		"/assets/app.js":           true,
		"/index.html":              false,
		"/favicon.ico":             false,
		"/assets/../index.html":    false,
	} {
		if got := isFingerprinted(path); got != want {
			t.Fatalf("%s is treated as fingerprinted: %v, want %v", path, got, want)
		}
	}

	// The shipping policy is the same function with the development override off, so
	// it is checked that way over files the test owns. Reaching it through the
	// embedded copy would mean depending on a hashed name that changes on every
	// build, which is a test that fails for reasons that have nothing to do with it.
	dir := t.TempDir()
	sub := filepath.Join(dir, "assets")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "index-abc123.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	shipping := fileHandler(os.DirFS(dir), false)
	// The document is asked for as "/", not as "/index.html". The file server
	// canonicalises the latter to a redirect, so asking for it would be testing the
	// redirect rather than the cache policy.
	for path, wantImmutable := range map[string]bool{
		"/assets/index-abc123.js": true,
		"/":                       false,
	} {
		recorder := httptest.NewRecorder()
		shipping.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s answered %d", path, recorder.Code)
		}
		cache := recorder.Header().Get("Cache-Control")
		if wantImmutable && !strings.Contains(cache, "immutable") {
			t.Fatalf("a built asset is cached as %q (status %d)", cache, recorder.Code)
		}
		if !wantImmutable && !strings.Contains(cache, "no-cache") {
			t.Fatalf("the document is cached as %q", cache)
		}
	}
}

// Nothing served from a directory is cached, including the assets, because in
// development every file there is being edited.
func TestNothingServedFromADirectoryIsCached(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "assets")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "index-abc123.js"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler, err := StaticHandlerFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/assets/index-abc123.js", "/"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if cache := recorder.Header().Get("Cache-Control"); cache != "no-store" {
			t.Fatalf("%s is served from disk as %q", path, cache)
		}
	}
}

// A stop is arranged by cancelling the context, and the listener is released even if
// the shutdown below it fails, or the next run cannot bind.
func TestAStopReleasesThePort(t *testing.T) {
	listener, err := LoopbackListener()
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(Config{Listener: listener, Token: token, Stream: &testStream{}})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Start(ctx) }()

	// Wait for it to be answering before stopping it, so this tests a stop and not a
	// start that never happened.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if response, err := http.Get("http://" + address + healthPath); err == nil {
			response.Body.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stopping reported %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stopping took longer than the budget")
	}

	// The port is free, so the next server can take it.
	reopened, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("the port was not released: %v", err)
	}
	reopened.Close()
}

// The server is built before it is started, so a caller holding one can put the token
// in a link before anything is listening. That closes the window where a listener
// exists that nobody has the token for.
func TestNewDoesNotStartAnything(t *testing.T) {
	server, _ := newTestServer(t, nil)
	if server.Token() == "" {
		t.Fatal("the token is not available before serving")
	}
	if server.Addr() == "" {
		t.Fatal("the address is not available before serving")
	}
}
