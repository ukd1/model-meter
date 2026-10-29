package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAnthropicUsage(t *testing.T) {
	cases := []struct {
		name            string
		events          []string
		in, out, cached int64
	}{
		{"JSON with caches", []string{`{"type":"message","model":"m","usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":20,"cache_creation_input_tokens":10}}`}, 35, 7, 20},
		{"split cumulative stream", []string{`{"type":"message_start","message":{"type":"message","model":"m","usage":{"input_tokens":5,"output_tokens":0,"cache_read_input_tokens":20,"cache_creation_input_tokens":10}}}`, `{"type":"message_delta","usage":{"output_tokens":2}}`, `{"type":"message_delta","usage":{"output_tokens":7}}`, `{"type":"message_stop"}`}, 35, 7, 20},
		{"repeated full snapshots", []string{`{"type":"message","usage":{"input_tokens":5,"output_tokens":2,"cache_read_input_tokens":20,"cache_creation_input_tokens":10}}`, `{"type":"message_delta","usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":20,"cache_creation_input_tokens":10}}`}, 35, 7, 20},
		{"zero uncached input", []string{`{"type":"message","usage":{"input_tokens":0,"output_tokens":7,"cache_read_input_tokens":20,"cache_creation_input_tokens":15}}`}, 35, 7, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Meter{SSE: true}
			for _, event := range tc.events {
				for _, b := range []byte("event: ignored\r\ndata: " + event + "\r\n\r\n") {
					m.Write([]byte{b})
				}
			}
			if m.Input == nil || m.Output == nil || m.Cached == nil || *m.Input != tc.in || *m.Output != tc.out || *m.Cached != tc.cached {
				t.Fatalf("incorrect meter: %+v", m)
			}
		})
	}
}

func TestChatProtocolCompatibility(t *testing.T) {
	cases := []struct {
		name, path, request, response, contentType string
		anthropic                                  bool
		input, output, cached                      int
	}{
		{"messages JSON", "/v1/messages", `{"model":"m","max_tokens":8,"system":"PRIVATE SYSTEM","messages":[{"role":"user","content":"PRIVATE PROMPT"}]}`, `{"type":"message","model":"m","content":[{"type":"text","text":"PRIVATE RESPONSE"}],"usage":{"input_tokens":5,"output_tokens":7,"cache_read_input_tokens":20,"cache_creation_input_tokens":10}}`, "application/json", true, 35, 7, 20},
		{"messages SSE", "/v1/messages", `{"model":"m","max_tokens":8,"messages":[],"stream":true}`, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"type\":\"message\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":0,\"cache_read_input_tokens\":20,\"cache_creation_input_tokens\":10}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", "text/event-stream", true, 35, 7, 20},
		{"responses JSON", "/v1/responses", `{"model":"m","input":"PRIVATE PROMPT","max_output_tokens":8}`, `{"object":"response","model":"m","output":[],"usage":{"input_tokens":35,"output_tokens":7,"input_tokens_details":{"cached_tokens":20}}}`, "application/json", false, 35, 7, 20},
		{"responses SSE", "/v1/responses", `{"model":"m","input":"PRIVATE PROMPT","stream":true}`, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"model\":\"m\",\"usage\":null}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"m\",\"usage\":{\"input_tokens\":35,\"output_tokens\":7,\"input_tokens_details\":{\"cached_tokens\":20}}}}\n\n", "text/event-stream", false, 35, 7, 20},
		{"legacy completions", "/v1/completions", `{"model":"m","prompt":"PRIVATE PROMPT","max_tokens":8}`, `{"model":"m","choices":[{"text":"PRIVATE RESPONSE"}],"usage":{"prompt_tokens":35,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":20}}}`, "application/json", false, 35, 7, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if string(body) != tc.request {
					t.Error("protocol body changed")
				}
				if r.URL.Path != tc.path {
					t.Error("route changed")
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("X-Private") != "" {
					t.Error("private headers leaked")
				}
				if tc.anthropic {
					if r.Header.Get("X-Api-Key") != "upstream-only" || r.Header.Get("Authorization") != "" {
						t.Error("wrong Anthropic upstream auth")
					}
					if r.Header.Get("Anthropic-Version") != "2023-06-01" || r.Header.Get("Anthropic-Beta") != "test-beta" {
						t.Error("protocol headers lost")
					}
				} else {
					if r.Header.Get("Authorization") != "Bearer upstream-only" || r.Header.Get("X-Api-Key") != "" {
						t.Error("wrong OpenAI upstream auth")
					}
					if r.Header.Get("OpenAI-Beta") != "responses-test" {
						t.Error("OpenAI beta header lost")
					}
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.Header().Set("Request-Id", "test-request")
				io.WriteString(w, tc.response)
			})
			a.upstreamKey = "upstream-only"
			r, _ := http.NewRequest("POST", s.URL+tc.path, strings.NewReader(tc.request))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Cookie", "PRIVATE")
			r.Header.Set("X-Private", "PRIVATE")
			r.Header.Set("OpenAI-Beta", "responses-test")
			if tc.anthropic {
				r.Header.Set("X-Api-Key", key)
				r.Header.Set("Anthropic-Version", "2023-06-01")
				r.Header.Set("Anthropic-Beta", "test-beta")
			} else {
				r.Header.Set("Authorization", "Bearer "+key)
			}
			res, e := s.Client().Do(r)
			if e != nil {
				t.Fatal(e)
			}
			body, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 || string(body) != tc.response || res.Header.Get("Request-Id") != "test-request" {
				t.Fatal("response passthrough failed")
			}
			awaitRows(t, a, 1)
			var in, out, cached int
			var model, path string
			if e := a.db.QueryRow(`SELECT input_tokens,output_tokens,cached_tokens,model,path FROM requests`).Scan(&in, &out, &cached, &model, &path); e != nil {
				t.Fatal(e)
			}
			if in != tc.input || out != tc.output || cached != tc.cached || model != "m" || path != tc.path {
				t.Fatal(in, out, cached, model, path)
			}
			logs := call(t, s, "/admin/requests", "admin-secret", "")
			data, _ := io.ReadAll(logs.Body)
			if strings.Contains(string(data), "PRIVATE") || strings.Contains(string(data), "upstream-only") {
				t.Fatal("private content persisted")
			}
		})
	}
}

func TestModelLookupAndAnthropicAuthentication(t *testing.T) {
	a, s, key := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models/model-id" {
			t.Error(r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "" || r.Header.Get("Authorization") != "" {
			t.Error("client key leaked")
		}
		io.WriteString(w, `{"id":"model-id"}`)
	})
	r, _ := http.NewRequest("GET", s.URL+"/v1/models/model-id?private=value", nil)
	r.Header.Set("X-Api-Key", key)
	res, e := s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	awaitRows(t, a, 1)
	var path string
	a.db.QueryRow(`SELECT path FROM requests`).Scan(&path)
	if path != "/v1/models/:model" {
		t.Fatal(path)
	}
	r.Header.Set("Authorization", "Bearer other-key")
	res, e = s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("conflicting keys accepted")
	}
	if call(t, s, "/admin/revoke", "admin-secret", `{"id":1}`).StatusCode != 200 {
		t.Fatal("revoke failed")
	}
	r.Header.Del("Authorization")
	res, e = s.Client().Do(r)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("revoked x-api-key accepted")
	}
}

func TestRouteParity(t *testing.T) {
	for path, method := range map[string]string{"/v1/models": "GET", "/v1/models/m": "GET", "/v1/chat/completions": "POST", "/v1/messages": "POST", "/v1/responses": "POST", "/v1/completions": "POST", "/v1/embeddings": "POST"} {
		_, got := proxyRoute(path)
		if got != method {
			t.Fatal(path, got)
		}
	}
	for _, path := range []string{"/v1/models/", "/v1/models/a/b", "/v1/messages/count_tokens", "/v1/responses/arbitrary"} {
		_, method := proxyRoute(path)
		if method != "" {
			t.Fatal("unexpected route", path)
		}
	}
}

func TestResponsesUsageSnapshots(t *testing.T) {
	m := &Meter{}
	for _, out := range []int{0, 2, 7} {
		m.parse([]byte(fmt.Sprintf(`{"type":"response.completed","response":{"model":"m","usage":{"input_tokens":35,"output_tokens":%d,"input_tokens_details":{"cached_tokens":20}}}}`, out)))
	}
	b, _ := json.Marshal([]*int64{m.Input, m.Output, m.Cached})
	if string(b) != "[35,7,20]" {
		t.Fatal(string(b))
	}
}
