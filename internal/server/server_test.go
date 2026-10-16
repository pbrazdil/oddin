package server

import (
	"bufio"
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"oddsboard/internal/board"
	"oddsboard/internal/feed"
	"oddsboard/internal/journal"
	"oddsboard/internal/market"
)

const (
	adminToken = "admin-secret-token"
	feedToken  = "feed-secret-token"
)

var def = market.Definition{
	Key: market.Key{MatchID: "match-1", MarketID: "winner"}, Event: "Falcons vs Wolves", Name: "Match winner",
	Outcomes: []market.OutcomeDef{{ID: "falcons", Name: "Falcons"}, {ID: "wolves", Name: "Wolves"}},
}

var specFeed = `[
  {"matchId":"match-1","marketId":"winner","seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}},
  {"matchId":"match-1","marketId":"winner","seq":42,"odds":{"falcons":1.80}},
  {"matchId":"match-1","marketId":"winner","seq":43,"status":"suspended"},
  {"matchId":"match-1","marketId":"winner","seq":44,"status":"open"},
  {"matchId":"match-1","marketId":"winner","seq":45,"odds":{"falcons":1.90,"wolves":1.95}}
]`

type env struct {
	srv *httptest.Server
	b   *board.Board
	sim *feed.Simulator
}

func newEnv(t *testing.T) env {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b, err := board.New([]market.Definition{def}, board.Config{Log: log}, journal.NewMemory(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sim := feed.NewSimulator(def, 1, func(raw []byte) { b.SubmitEvent(raw, board.SourceSimulator) })
	done := make(chan struct{})
	h := New(Options{
		Board:     b,
		Simulator: sim,
		Replayer:  feed.NewReplayer(func(raw []byte) (board.Ack, error) { return b.SubmitEvent(raw, board.SourceReplay) }),
		Assets: fstest.MapFS{
			"index.html":       {Data: []byte("<!doctype html><div id=root></div>")},
			"embed.js":         {Data: []byte("/* loader */")},
			"assets/app-1.js":  {Data: []byte("console.log(1)")},
			"demo/index.html":  {Data: []byte("<!doctype html>demo")},
			"demo/partner.css": {Data: []byte("body{}")},
		},
		AdminToken:     adminToken,
		FeedToken:      feedToken,
		FrameAncestors: "https://partner.example",
		Heartbeat:      50 * time.Millisecond,
		Log:            log,
		Done:           done,
	})
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		close(done)
		srv.Close()
		b.Close()
	})
	return env{srv, b, sim}
}

func (e env) do(t *testing.T, method, path, token, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestFeedRequiresToken(t *testing.T) {
	e := newEnv(t)
	for _, tok := range []string{"", "wrong", adminToken + "x", strings.ToUpper(feedToken)} {
		resp, _ := e.do(t, "POST", "/api/v1/feed/events", tok, specFeed)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d", tok, resp.StatusCode)
		}
	}
	if p, _ := e.b.Current(def.Key); p.State.Seq != 0 {
		t.Fatal("unauthenticated events were applied")
	}
}

func TestPostEventsAndReadState(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do(t, "POST", "/api/v1/feed/events", feedToken, specFeed)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	var res struct {
		Acks []board.Ack `json:"acks"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil || len(res.Acks) != 5 || res.Acks[4].Verdict != market.VerdictApplied {
		t.Fatalf("acks %s: %v", body, err)
	}

	resp, body = e.do(t, "GET", "/api/v1/markets/match-1/winner", "", "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "*" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("state: %d %v", resp.StatusCode, resp.Header)
	}
	var p board.Published
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if f, _ := p.State.Price("falcons"); f.String() != "1.90" || !p.State.Tradable || p.Version != 6 {
		t.Fatalf("state %s", body)
	}

	// Invalid bodies are rejected without touching the state.
	resp, _ = e.do(t, "POST", "/api/v1/feed/events", feedToken, `[{"seq":`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("broken body: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "POST", "/api/v1/feed/events", feedToken, strings.Repeat(" ", maxBody+1))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge body: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "GET", "/api/v1/markets/match-9/winner", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown market: %d", resp.StatusCode)
	}
}

func TestPostSnapshot(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do(t, "POST", "/api/v1/feed/snapshot", feedToken,
		`{"matchId":"match-1","marketId":"winner","seq":100,"status":"open","odds":{"falcons":1.70,"wolves":2.20}}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"verdict":"applied"`) {
		t.Fatalf("snapshot: %d %s", resp.StatusCode, body)
	}
	if p, _ := e.b.Current(def.Key); !p.State.Tradable || p.State.Seq != 100 {
		t.Fatalf("after snapshot: %+v", p.State)
	}
}

type sseEvent struct{ name, id, data string }

func readEvents(t *testing.T, r *bufio.Reader, n int) []sseEvent {
	t.Helper()
	var out []sseEvent
	var cur sseEvent
	for len(out) < n {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading stream: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if cur.name != "" {
				out = append(out, cur)
			}
			cur = sseEvent{}
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			cur.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

func nextState(t *testing.T, r *bufio.Reader) board.Published {
	t.Helper()
	for {
		ev := readEvents(t, r, 1)[0]
		if ev.name != "state" {
			continue
		}
		var p board.Published
		if err := json.Unmarshal([]byte(ev.data), &p); err != nil {
			t.Fatalf("state data %q: %v", ev.data, err)
		}
		if want := p.Epoch + "." + itoa(p.Version); ev.id != want {
			t.Fatalf("event id %q, want %q", ev.id, want)
		}
		return p
	}
}

func itoa(v uint64) string { b, _ := json.Marshal(v); return string(b) }

func TestStreamDeliversFullStatesAndHeartbeats(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/v1/markets/match-1/winner/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("headers %v", resp.Header)
	}
	r := bufio.NewReader(resp.Body)

	// The first state arrives immediately, without waiting for a change.
	first := nextState(t, r)
	if first.State.Seq != 0 || first.State.Tradable {
		t.Fatalf("initial state %+v", first.State)
	}

	e.do(t, "POST", "/api/v1/feed/events", feedToken, `{"matchId":"match-1","marketId":"winner","seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}}`)
	p := nextState(t, r)
	if p.Version <= first.Version || p.State.Seq != 41 || !p.State.Tradable {
		t.Fatalf("update %+v", p)
	}

	// Heartbeats carry the current version.
	for {
		ev := readEvents(t, r, 1)[0]
		if ev.name == "heartbeat" {
			if !strings.Contains(ev.data, `"version":`+itoa(p.Version)) || !strings.Contains(ev.data, `"serverTime"`) {
				t.Fatalf("heartbeat %s", ev.data)
			}
			break
		}
	}

	// A reconnecting client gets the current state straight away.
	req2, _ := http.NewRequestWithContext(ctx, "GET", e.srv.URL+"/api/v1/markets/match-1/winner/stream", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if again := nextState(t, bufio.NewReader(resp2.Body)); again.Version != p.Version || again.State.Seq != 41 {
		t.Fatalf("reconnect state %+v", again)
	}
}

func TestEmbedPageIsFramable(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do(t, "GET", "/embed/match-1/winner", "", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "root") {
		t.Fatalf("embed page: %d %s", resp.StatusCode, body)
	}
	if v := resp.Header.Get("X-Frame-Options"); v != "" {
		t.Fatalf("X-Frame-Options %q would block embedding", v)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors https://partner.example") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP %q", csp)
	}
	if resp.Header.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Fatal("embed page lacks CORP for COEP hosts")
	}

	resp, _ = e.do(t, "GET", "/embed?theme=dark", "", "")
	if resp.Request.URL.Path != "/embed/match-1/winner" || resp.Request.URL.RawQuery != "theme=dark" {
		t.Fatalf("default embed redirect went to %s", resp.Request.URL)
	}
	resp, _ = e.do(t, "GET", "/embed/match-9/winner", "", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown market embed: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, "GET", "/embed.js", "", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("embed.js: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, _ = e.do(t, "GET", "/assets/app-1.js", "", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("asset: %d", resp.StatusCode)
	}
	for _, p := range []string{"/assets/../index.html", "/assets/nope.js", "/demo/nope.html"} {
		if resp, _ := e.do(t, "GET", p, "", ""); resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMovedPermanently {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp, body := e.do(t, "GET", "/", "", ""); resp.StatusCode != http.StatusOK || body != "<!doctype html>demo" {
		t.Fatalf("root: %d %s", resp.StatusCode, body)
	}
}

func TestAdminRequiresAdminToken(t *testing.T) {
	e := newEnv(t)
	paths := []struct{ method, path string }{
		{"GET", "/api/v1/admin/session"},
		{"GET", "/api/v1/admin/markets/match-1/winner/history"},
		{"GET", "/api/v1/admin/markets/match-1/winner/at?t=2026-01-01T00:00:00Z"},
		{"POST", "/api/v1/admin/markets/match-1/winner/reset"},
		{"POST", "/api/v1/admin/markets/match-1/winner/replay"},
		{"GET", "/api/v1/admin/simulator"},
		{"PUT", "/api/v1/admin/simulator"},
		{"POST", "/api/v1/admin/simulator/interrupt"},
		{"GET", "/api/v1/admin/verify"},
		{"DELETE", "/api/v1/admin/replay"},
	}
	for _, p := range paths {
		for _, tok := range []string{"", feedToken, "nope"} {
			resp, _ := e.do(t, p.method, p.path, tok, "{}")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %q: %d", p.method, p.path, tok, resp.StatusCode)
			}
			if resp.Header.Get("Access-Control-Allow-Origin") != "" {
				t.Errorf("%s %s is CORS-enabled", p.method, p.path)
			}
		}
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/session", adminToken, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("admin session: %d", resp.StatusCode)
	}
}

func TestAdminReplayHistoryAndStateAt(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do(t, "POST", "/api/v1/admin/markets/match-1/winner/replay", adminToken,
		`{"events":`+specFeed+`,"intervalMs":0,"reset":true,"stopSimulator":true}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("replay: %d %s", resp.StatusCode, body)
	}
	if p, _ := e.b.Current(def.Key); p.State.Seq != 45 {
		t.Fatalf("after replay seq %d", p.State.Seq)
	}

	resp, body = e.do(t, "GET", "/api/v1/admin/markets/match-1/winner/history?after=0&limit=100", adminToken, "")
	var hist struct {
		Records []journal.Record `json:"records"`
	}
	if err := json.Unmarshal([]byte(body), &hist); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("history: %d %v", resp.StatusCode, err)
	}
	var summaries []string
	for _, r := range hist.Records {
		if r.Source == string(board.SourceReplay) && r.Kind == journal.KindEvent {
			summaries = append(summaries, r.Summary)
		}
	}
	if len(summaries) != 5 || summaries[2] != "Market suspended; trading locked: market suspended" {
		t.Fatalf("replayed summaries %q", summaries)
	}

	suspendedAt := hist.Records[len(hist.Records)-3].At // seq 43
	resp, body = e.do(t, "GET", "/api/v1/admin/markets/match-1/winner/at?t="+suspendedAt.Format(time.RFC3339Nano), adminToken, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"status":"suspended"`) {
		t.Fatalf("state at %v: %d %s", suspendedAt, resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "GET", "/api/v1/admin/markets/match-1/winner/at?t=yesterday", adminToken, ""); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad time: %d", resp.StatusCode)
	}

	resp, body = e.do(t, "GET", "/api/v1/admin/verify", adminToken, "")
	if !strings.Contains(body, `"chainOk":true`) || !strings.Contains(body, `"replayOk":true`) {
		t.Fatalf("verify: %s", body)
	}
}

func TestAdminSimulatorControls(t *testing.T) {
	e := newEnv(t)
	resp, body := e.do(t, "PUT", "/api/v1/admin/simulator", adminToken, `{"running":true,"intervalMs":500,"dropRate":0.5}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"dropRate":0.5`) || !strings.Contains(body, `"running":true`) {
		t.Fatalf("configure: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, "POST", "/api/v1/admin/simulator/interrupt", adminToken, `{"seconds":5}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "interruptedUntil") {
		t.Fatalf("interrupt: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, "POST", "/api/v1/admin/simulator/interrupt", adminToken, `{"seconds":-1}`); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("negative interrupt: %d", resp.StatusCode)
	}
}

func TestLongPollFallback(t *testing.T) {
	e := newEnv(t)
	get := func(query string) (board.Published, time.Duration, *http.Response) {
		t.Helper()
		start := time.Now()
		resp, body := e.do(t, "GET", "/api/v1/markets/match-1/winner"+query, "", "")
		var p board.Published
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		return p, time.Since(start), resp
	}

	cur, _, resp := get("")
	if _, err := time.Parse(time.RFC3339Nano, resp.Header.Get("X-Server-Time")); err != nil {
		t.Fatalf("X-Server-Time %q", resp.Header.Get("X-Server-Time"))
	}
	if !strings.Contains(resp.Header.Get("Access-Control-Expose-Headers"), "X-Server-Time") {
		t.Fatal("X-Server-Time not exposed to cross-origin clients")
	}
	tag := cur.Epoch + "." + itoa(cur.Version)

	// A client that is behind gets the current state at once.
	if p, d, _ := get("?after=" + cur.Epoch + ".0&wait=5000"); p.Version != cur.Version || d > time.Second {
		t.Fatalf("behind client: v%d after %v", p.Version, d)
	}

	// An up-to-date client waits until the timeout and gets the same state.
	if p, d, _ := get("?after=" + tag + "&wait=300"); p.Version != cur.Version || d < 250*time.Millisecond {
		t.Fatalf("idle poll: v%d after %v", p.Version, d)
	}

	// A change wakes the waiting request up early.
	go func() {
		time.Sleep(200 * time.Millisecond)
		e.b.SubmitEvent([]byte(`{"matchId":"match-1","marketId":"winner","seq":41,"status":"open","odds":{"falcons":1.85,"wolves":2.05}}`), board.SourceFeed)
	}()
	p, d, _ := get("?after=" + tag + "&wait=5000")
	if p.Version != cur.Version+1 || p.State.Seq != 41 || d > 2*time.Second {
		t.Fatalf("woken poll: v%d seq %d after %v", p.Version, p.State.Seq, d)
	}

	// Waits are capped.
	if _, d, _ := get("?after=" + p.Epoch + "." + itoa(p.Version) + "&wait=999999"); d > 6*time.Second {
		t.Fatalf("uncapped wait %v", d)
	}
}
