package portainer

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const token = "ptr_test"

// fake answers like Portainer's API and records what it was sent.
func fake(t *testing.T, tlsServer bool, handle func(w http.ResponseWriter, r *http.Request, body map[string]any)) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-KEY") != token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			json.Unmarshal(b, &body)
		}
		handle(w, r, body)
	})
	var srv *httptest.Server
	if tlsServer {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv
}

func client(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(srv.URL+"/", token, 2, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestStacksKeepsOnlyComposeStacksOfTheEnvironment(t *testing.T) {
	srv := fake(t, false, func(w http.ResponseWriter, r *http.Request, _ map[string]any) {
		if r.URL.Path != "/api/stacks" || !strings.Contains(r.URL.Query().Get("filters"), `"EndpointID":2`) {
			t.Errorf("request = %s", r.URL)
		}
		json.NewEncoder(w).Encode([]map[string]any{
			{"Id": 1, "Name": "app", "Type": TypeCompose, "EndpointId": 2, "Env": []map[string]string{{"name": "A", "value": "b"}}},
			{"Id": 2, "Name": "swarm", "Type": TypeSwarm, "EndpointId": 2},
			{"Id": 3, "Name": "elsewhere", "Type": TypeCompose, "EndpointId": 9},
		})
	})
	got, err := client(t, srv).Stacks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != 1 || got[0].Env[0].Value != "b" {
		t.Fatalf("stacks = %+v", got)
	}
}

func TestUpdateFileStackSendsTheWholeEnvAndNeverPulls(t *testing.T) {
	var sent map[string]any
	srv := fake(t, false, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/stacks/7" || r.URL.Query().Get("endpointId") != "2" {
			t.Errorf("request = %s %s", r.Method, r.URL)
		}
		sent = body
	})
	err := client(t, srv).UpdateFileStack(context.Background(), 7, "services: {}", []Pair{{Name: "TAG", Value: "1.2"}, {Name: "KEEP", Value: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if sent["StackFileContent"] != "services: {}" || sent["Prune"] != false || sent["RepullImageAndRedeploy"] != false {
		t.Fatalf("sent = %v", sent)
	}
	if env := sent["Env"].([]any); len(env) != 2 {
		t.Fatalf("env = %v", env)
	}
	// An empty list must be sent as a list: Portainer would otherwise drop it.
	if err := client(t, srv).UpdateFileStack(context.Background(), 7, "", nil); err != nil {
		t.Fatal(err)
	}
	if env, ok := sent["Env"].([]any); !ok || len(env) != 0 {
		t.Fatalf("env = %#v", sent["Env"])
	}
}

func TestRedeployGitStackKeepsItsReference(t *testing.T) {
	var sent map[string]any
	srv := fake(t, false, func(w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.URL.Path != "/api/stacks/7/git/redeploy" {
			t.Errorf("path = %s", r.URL.Path)
		}
		sent = body
	})
	s := Stack{ID: 7, Name: "app", GitConfig: &GitConfig{URL: "https://example.org/r.git", ReferenceName: "refs/heads/main"}}
	if !s.GitBased() {
		t.Fatal("not git based")
	}
	if err := client(t, srv).RedeployGitStack(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if sent["RepositoryReferenceName"] != "refs/heads/main" || sent["RepullImageAndRedeploy"] != false {
		t.Fatalf("sent = %v", sent)
	}
}

func TestErrorsCarryPortainersMessageAndNotTheToken(t *testing.T) {
	srv := fake(t, false, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"message": "Access denied", "details": "not your stack"})
	})
	_, err := client(t, srv).Stack(context.Background(), 7)
	if err == nil || !strings.Contains(err.Error(), "HTTP 403: Access denied not your stack") || strings.Contains(err.Error(), token) {
		t.Fatalf("err = %v", err)
	}
}

func TestCertificates(t *testing.T) {
	srv := fake(t, true, func(w http.ResponseWriter, _ *http.Request, _ map[string]any) {
		json.NewEncoder(w).Encode("content")
	})
	ctx := context.Background()
	t.Run("an unknown certificate is refused", func(t *testing.T) {
		c, _ := New(srv.URL, token, 2, false, "")
		if _, err := c.Stack(ctx, 7); err == nil {
			t.Fatal("no error")
		}
	})
	t.Run("a CA file trusts it", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		block := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
		if err := os.WriteFile(path, block, 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := New(srv.URL, token, 2, false, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Stack(ctx, 7); err != nil && strings.Contains(err.Error(), "certificate") {
			t.Fatal(err)
		}
	})
	t.Run("a bad CA file is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "ca.pem")
		os.WriteFile(path, []byte("not a certificate"), 0o600)
		if _, err := New(srv.URL, token, 2, false, path); err == nil {
			t.Fatal("no error")
		}
	})
}
