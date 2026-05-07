package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPostKnock_HappyPath(t *testing.T) {
	ts, _ := newTestServer(t)
	body := `{"from":"alice","topic":"ops","kind":"need","subject":"hello"}`
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/knocks", body))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, buf)
	}
	var got Knock
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.From != "alice" {
		t.Errorf("From=%q, want alice", got.From)
	}
	if got.Topic != "ops" {
		t.Errorf("Topic=%q, want ops", got.Topic)
	}
	if got.Kind != "need" {
		t.Errorf("Kind=%q, want need", got.Kind)
	}
	if got.Subject != "hello" {
		t.Errorf("Subject=%q, want hello", got.Subject)
	}
	if len(got.ID) != 28 {
		t.Errorf("len(ID)=%d, want 28", len(got.ID))
	}
	if got.Ts.IsZero() {
		t.Error("Ts should be assigned")
	}
}

func TestPostKnock_Defaults(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/knocks", `{"from":"alice"}`))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got Knock
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Topic != "default" {
		t.Errorf("Topic=%q, want default (when omitted)", got.Topic)
	}
	if got.Kind != "info" {
		t.Errorf("Kind=%q, want info (when omitted)", got.Kind)
	}
}

func TestPostKnock_BadJSON(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/knocks", `{not-json}`))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "bad json") {
		t.Errorf("body=%q, want contains 'bad json'", string(body))
	}
}

func TestPostKnock_OversizedBody(t *testing.T) {
	ts, _ := newTestServer(t)
	// JSON is valid; only the size triggers the cap. Pad subject past 1MB.
	big := strings.Repeat("x", (1<<20)+1024)
	body := `{"from":"alice","subject":"` + big + `"}`
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/knocks", body))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s; want 413", resp.StatusCode, buf)
	}
}

func TestPostKnock_MissingFrom(t *testing.T) {
	ts, _ := newTestServer(t)
	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/knocks", `{"topic":"ops"}`))
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "from is required") {
		t.Errorf("body=%q, want 'from is required'", string(body))
	}
}

func TestListKnocks_Filters(t *testing.T) {
	ts, srv := newTestServer(t)
	// Seed via the store directly to avoid relying on POST for setup.
	srv.store.Add(Knock{ID: "id001", From: "a", Topic: "ops", To: "alice"})
	srv.store.Add(Knock{ID: "id002", From: "a", Topic: "marketing", To: "alice"})
	srv.store.Add(Knock{ID: "id003", From: "a", Topic: "ops", To: ""})

	t.Run("no filter returns all", func(t *testing.T) {
		got := getKnocks(t, ts.URL+"/v1/knocks")
		if len(got) != 3 {
			t.Errorf("len=%d, want 3 (%v)", len(got), ids(got))
		}
	})
	t.Run("topic filter", func(t *testing.T) {
		got := getKnocks(t, ts.URL+"/v1/knocks?topic=ops")
		if len(got) != 2 {
			t.Errorf("len=%d, want 2 (%v)", len(got), ids(got))
		}
	})
	t.Run("to filter includes broadcasts", func(t *testing.T) {
		got := getKnocks(t, ts.URL+"/v1/knocks?to=alice")
		if len(got) != 3 {
			t.Errorf("len=%d, want 3 incl broadcast (%v)", len(got), ids(got))
		}
	})
	t.Run("since is exclusive", func(t *testing.T) {
		got := getKnocks(t, ts.URL+"/v1/knocks?since=id001")
		if len(got) != 2 || got[0].ID != "id002" {
			t.Errorf("got %v, want [id002, id003]", ids(got))
		}
	})
	t.Run("limit caps", func(t *testing.T) {
		got := getKnocks(t, ts.URL+"/v1/knocks?limit=1")
		if len(got) != 1 {
			t.Errorf("len=%d, want 1", len(got))
		}
	})
}

func TestKnocks_RoundTrip(t *testing.T) {
	ts, _ := newTestServer(t)

	resp := doRequest(t, bearerReq(t, http.MethodPost, ts.URL+"/v1/knocks", `{"from":"alice","topic":"ops","subject":"hello"}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("post status=%d", resp.StatusCode)
	}

	got := getKnocks(t, ts.URL+"/v1/knocks?topic=ops")
	if len(got) != 1 {
		t.Fatalf("after POST: GET len=%d, want 1", len(got))
	}
	if got[0].Subject != "hello" || got[0].From != "alice" {
		t.Errorf("round-trip mismatch: %+v", got[0])
	}
}

func doRequest(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL.String(), err)
	}
	return resp
}

func getKnocks(t *testing.T, url string) []Knock {
	t.Helper()
	resp := doRequest(t, bearerReq(t, http.MethodGet, url, ""))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: status=%d, body=%s", url, resp.StatusCode, buf)
	}
	var out []Knock
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}
