package docker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/moby/moby/client"
)

// fakeDaemon answers the few Engine API calls the Client makes. Paths missing
// from routes get the daemon's real error shape with status miss (a 404 by
// default), so these tests exercise the moby client's own error mapping,
// which the Fake engine cannot.
func fakeDaemon(t *testing.T, routes map[string]string, miss ...int) *Client {
	t.Helper()
	status := http.StatusNotFound
	if len(miss) > 0 {
		status = miss[0]
	}
	version := regexp.MustCompile(`^/v[0-9.]+`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + version.ReplaceAllString(r.URL.Path, "")
		w.Header().Set("Content-Type", "application/json")
		if body, ok := routes[key]; ok {
			w.Write([]byte(body))
			return
		}
		w.WriteHeader(status)
		w.Write([]byte(`{"message":"No such object: ` + r.URL.Path + `"}`))
	}))
	t.Cleanup(srv.Close)
	c, err := client.New(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersion("1.47"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &Client{c: c}
}

// A container removed between list and inspect (Compose recreating it while
// verify or discovery runs) is skipped, not an error.
func TestContainersSkipsRemovedContainer(t *testing.T) {
	d := fakeDaemon(t, map[string]string{
		"GET /containers/json": `[{"Id":"gone","State":"running"},{"Id":"kept","State":"running"}]`,
		"GET /containers/kept/json": `{"Id":"kept","Name":"/kept","Image":"sha256:abc","Created":"2026-10-28T01:32:00Z",
			"Config":{"Image":"nginx:1.27","Labels":{"a":"b"}},"State":{"Status":"running"}}`,
	})
	got, err := d.Containers(context.Background())
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if len(got) != 1 || got[0].ID != "kept" || got[0].Name != "kept" {
		t.Fatalf("got %+v, want only kept", got)
	}
}

func TestImageNotFound(t *testing.T) {
	d := fakeDaemon(t, nil)
	if _, err := d.Image(context.Background(), "sha256:missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Image: got %v, want ErrNotFound", err)
	}
}

func TestUntagMissingTag(t *testing.T) {
	d := fakeDaemon(t, nil)
	if err := d.Untag(context.Background(), "ddum/pin:gone"); err != nil {
		t.Fatalf("Untag: got %v, want nil", err)
	}
}

// Errors other than not-found still surface.
func TestContainersInspectFailure(t *testing.T) {
	d := fakeDaemon(t, map[string]string{"GET /containers/json": `[{"Id":"x"}]`}, http.StatusInternalServerError)
	if _, err := d.Containers(context.Background()); err == nil {
		t.Fatal("Containers: want an error for a 500")
	}
}
