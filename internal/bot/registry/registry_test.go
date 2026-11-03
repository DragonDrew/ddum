package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// testRegistry is an in-process registry. hook, when set, may answer a
// request itself (return true) before the registry sees it.
type testRegistry struct {
	srv      *httptest.Server
	host     string
	requests atomic.Int64
	mu       sync.Mutex
	hook     func(w http.ResponseWriter, r *http.Request) bool
}

func newRegistry(t *testing.T) *testRegistry {
	t.Helper()
	tr := &testRegistry{}
	reg := ggcrregistry.New(ggcrregistry.Logger(log.New(io.Discard, "", 0)))
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.requests.Add(1)
		tr.mu.Lock()
		hook := tr.hook
		tr.mu.Unlock()
		if hook != nil && hook(w, r) {
			return
		}
		reg.ServeHTTP(w, r)
	}))
	t.Cleanup(tr.srv.Close)
	tr.host = strings.TrimPrefix(tr.srv.URL, "http://")
	return tr
}

func (tr *testRegistry) setHook(h func(http.ResponseWriter, *http.Request) bool) {
	tr.mu.Lock()
	tr.hook = h
	tr.mu.Unlock()
}

func (tr *testRegistry) pushImage(t *testing.T, repoTag string, labels map[string]string) v1.Hash {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	if labels != nil {
		cf, _ := img.ConfigFile()
		cf = cf.DeepCopy()
		cf.Config.Labels = labels
		if img, err = mutate.ConfigFile(img, cf); err != nil {
			t.Fatal(err)
		}
	}
	ref, err := name.NewTag(tr.host+"/"+repoTag, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	return d
}

func client(now func() time.Time) *Client {
	return New(Config{ConcurrencyPerHost: 2, RequestGap: time.Millisecond, Insecure: true, Now: now})
}

func TestRequestsToOneHostAreSpaced(t *testing.T) {
	reg := newRegistry(t)
	reg.pushImage(t, "app:1.0", nil)
	const gap = 40 * time.Millisecond
	c := New(Config{ConcurrencyPerHost: 4, RequestGap: gap, Insecure: true})
	before := reg.requests.Load()
	start := time.Now()
	for i := 0; i < 3; i++ {
		if _, err := c.Digest(context.Background(), reg.host+"/app", "1.0"); err != nil {
			t.Fatal(err)
		}
	}
	// One ping, then a HEAD per call: four requests, three gaps.
	if n := reg.requests.Load() - before; n != 4 {
		t.Fatalf("requests = %d, want 4", n)
	}
	if elapsed := time.Since(start); elapsed < 3*gap {
		t.Fatalf("elapsed %s, want at least %s between requests", elapsed, 3*gap)
	}
}

func TestOneTokenServesEveryRepositoryOfAHost(t *testing.T) {
	reg := newRegistry(t)
	reg.pushImage(t, "a:1.0", nil)
	reg.pushImage(t, "b:1.0", nil)
	reg.pushImage(t, "c:2.0", nil)
	var pings, tokens atomic.Int64
	challenge := `Bearer realm="http://` + reg.host + `/token",service="test"`
	reg.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.URL.Path == "/v2/":
			pings.Add(1)
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/token":
			tokens.Add(1)
			io.WriteString(w, `{"token":"t"}`)
		case r.Header.Get("Authorization") != "Bearer t":
			w.Header().Set("WWW-Authenticate", challenge)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			return false
		}
		return true
	})
	c := client(nil)
	ctx := context.Background()
	for _, repo := range []string{"a", "b", "c"} {
		if _, err := c.Tags(ctx, reg.host+"/"+repo); err != nil {
			t.Fatal(err)
		}
	}
	for _, rt := range []string{"a:1.0", "b:1.0", "c:2.0"} {
		repo, tag, _ := strings.Cut(rt, ":")
		if _, err := c.Digest(ctx, reg.host+"/"+repo, tag); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Metadata(ctx, reg.host+"/c", "2.0"); err != nil {
		t.Fatal(err)
	}
	if p, tk := pings.Load(), tokens.Load(); p != 1 || tk != 1 {
		t.Fatalf("pings = %d, tokens = %d; want 1 each for the host", p, tk)
	}
}

func TestSharedTransportIsRebuiltAfterMaxAge(t *testing.T) {
	reg := newRegistry(t)
	reg.pushImage(t, "app:1.0", nil)
	var pings atomic.Int64
	reg.setHook(func(_ http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/v2/" {
			pings.Add(1)
		}
		return false
	})
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := client(func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if _, err := c.Digest(context.Background(), reg.host+"/app", "1.0"); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(authMaxAge)
	if _, err := c.Digest(context.Background(), reg.host+"/app", "1.0"); err != nil {
		t.Fatal(err)
	}
	if p := pings.Load(); p != 2 {
		t.Fatalf("pings = %d, want 2 (first use, then after authMaxAge)", p)
	}
}

func TestPacerHonoursCancellation(t *testing.T) {
	p := &pacer{next: http.DefaultTransport, gap: time.Hour, at: map[string]time.Time{}}
	p.at["example.test"] = time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://example.test/v2/", nil)
	if _, err := p.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestDigestFollowsAMovedTag(t *testing.T) {
	reg := newRegistry(t)
	c := client(nil)
	ctx := context.Background()
	d1 := reg.pushImage(t, "app:latest", nil)
	got, err := c.Digest(ctx, reg.host+"/app", "latest")
	if err != nil || got != d1.String() {
		t.Fatalf("digest = %s, %v; want %s", got, err, d1)
	}
	d2 := reg.pushImage(t, "app:latest", nil)
	if got, _ := c.Digest(ctx, reg.host+"/app", "latest"); got != d2.String() || d1 == d2 {
		t.Fatalf("moved tag: got %s, want %s", got, d2)
	}
}

func TestDigestUsesHEAD(t *testing.T) {
	reg := newRegistry(t)
	reg.pushImage(t, "app:1.0", nil)
	var methods []string
	reg.setHook(func(_ http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/manifests/") {
			methods = append(methods, r.Method)
		}
		return false
	})
	if _, err := client(nil).Digest(context.Background(), reg.host+"/app", "1.0"); err != nil {
		t.Fatal(err)
	}
	for _, m := range methods {
		if m != http.MethodHead {
			t.Fatalf("manifest requests = %v; digest checks must use HEAD only", methods)
		}
	}
}

func TestMultiArchIndexDigest(t *testing.T) {
	reg := newRegistry(t)
	idx, err := random.Index(64, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := name.NewTag(reg.host+"/multi:1.0", name.Insecure)
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	want, _ := idx.Digest()
	got, err := client(nil).Digest(context.Background(), reg.host+"/multi", "1.0")
	if err != nil || got != want.String() {
		t.Fatalf("index digest = %s, %v; want %s (the top-level digest)", got, err, want)
	}
}

func TestTagsAndNotFound(t *testing.T) {
	reg := newRegistry(t)
	for _, tag := range []string{"1.0.0", "1.1.0", "latest"} {
		reg.pushImage(t, "app:"+tag, nil)
	}
	c := client(nil)
	tags, err := c.Tags(context.Background(), reg.host+"/app")
	sort.Strings(tags)
	if err != nil || strings.Join(tags, ",") != "1.0.0,1.1.0,latest" {
		t.Fatalf("tags = %v, %v", tags, err)
	}
	if _, err := c.Digest(context.Background(), reg.host+"/app", "9.9.9"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing tag err = %v, want ErrNotFound", err)
	}
}

// TestTagsAfterFollowsPushOrderPages serves a ghcr.io-style tag list: push
// order, pages linked by a relative Link header, and no tags after a last
// that does not exist.
func TestTagsAfterFollowsPushOrderPages(t *testing.T) {
	reg := newRegistry(t)
	reg.pushImage(t, "big:seed", nil)
	var pushed []string
	for i := range 2500 {
		pushed = append(pushed, fmt.Sprintf("commit-%04d", 2500-i)) // push order, not lexical
	}
	var pages atomic.Int64
	reg.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v2/big/tags/list" {
			return false
		}
		pages.Add(1)
		q := r.URL.Query()
		n, _ := strconv.Atoi(q.Get("n"))
		from := 0
		if last := q.Get("last"); last != "" {
			from = slices.Index(pushed, last) + 1
			if from == 0 {
				from = len(pushed) // unknown last: nothing
			}
		}
		to := min(from+n, len(pushed))
		if to < len(pushed) {
			w.Header().Set("Link", fmt.Sprintf(`</v2/big/tags/list?last=%s&n=%d>; rel="next"`, pushed[to-1], n))
		}
		json.NewEncoder(w).Encode(map[string]any{"name": "big", "tags": pushed[from:to]})
		return true
	})
	c := client(nil)
	repo := reg.host + "/big"

	got, err := c.TagsAfter(context.Background(), repo, pushed[1])
	if err != nil || !slices.Equal(got, pushed[2:]) || pages.Load() != 3 {
		t.Fatalf("after %s: %d tags, %d pages, %v; want %d tags over 3 pages", pushed[1], len(got), pages.Load(), err, len(pushed)-2)
	}
	if got, err := c.TagsAfter(context.Background(), repo, pushed[len(pushed)-2]); err != nil || !slices.Equal(got, pushed[len(pushed)-1:]) {
		t.Fatalf("after the second-to-last tag: %v, %v", got, err)
	}
	if got, err := c.TagsAfter(context.Background(), repo, "deleted"); err != nil || len(got) != 0 {
		t.Fatalf("after an unknown tag: %v, %v; want none", got, err)
	}

	reg.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	if _, err := c.TagsAfter(context.Background(), repo, pushed[0]); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: err = %v, want ErrRateLimited", err)
	}
}

func TestPushOrdered(t *testing.T) {
	c := New(Config{})
	for repo, want := range map[string]bool{
		"ghcr.io/immich-app/immich-server": true,
		"lscr.io/linuxserver/sonarr":       true,
		"nginx":                            false,
		"quay.io/prometheus/node-exporter": false,
	} {
		if got := c.PushOrdered(repo); got != want {
			t.Errorf("PushOrdered(%s) = %v, want %v", repo, got, want)
		}
	}
}

func TestRateLimitBacksOffPerRegistry(t *testing.T) {
	reg := newRegistry(t)
	reg.pushImage(t, "app:1.0", nil)
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	c := client(func() time.Time { return now })
	reg.setHook(func(w http.ResponseWriter, r *http.Request) bool {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	if _, err := c.Digest(context.Background(), reg.host+"/app", "1.0"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
	reg.setHook(nil)
	before := reg.requests.Load()
	if _, err := c.Tags(context.Background(), reg.host+"/app"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("inside Retry-After: err = %v, want ErrRateLimited without a request", err)
	}
	if reg.requests.Load() != before {
		t.Fatal("a rate-limited registry must not be contacted until Retry-After passes")
	}
	now = now.Add(31 * time.Second)
	if _, err := c.Digest(context.Background(), reg.host+"/app", "1.0"); err != nil {
		t.Fatalf("after Retry-After: %v", err)
	}
}

func TestConcurrencyPerHost(t *testing.T) {
	reg := newRegistry(t)
	reg.pushImage(t, "app:1.0", nil)
	var cur, peak atomic.Int64
	reg.setHook(func(_ http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodHead {
			n := cur.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			cur.Add(-1)
		}
		return false
	})
	c := client(nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Digest(context.Background(), reg.host+"/app", "1.0")
		}()
	}
	wg.Wait()
	if peak.Load() > 2 {
		t.Fatalf("peak concurrent requests = %d, want ≤ 2", peak.Load())
	}
}

func TestMetadataAndReleaseURL(t *testing.T) {
	reg := newRegistry(t)
	reg.pushImage(t, "immich:v1.141.0", map[string]string{keySource: "https://github.com/immich-app/immich.git"})
	meta, err := client(nil).Metadata(context.Background(), reg.host+"/immich", "v1.141.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := ReleaseURL(meta, "version", "v1.141.0"); got != "https://github.com/immich-app/immich/releases/tag/v1.141.0" {
		t.Fatalf("release URL = %q", got)
	}
}

func TestReleaseURL(t *testing.T) {
	cases := []struct {
		meta map[string]string
		kind string
		want string
	}{
		{map[string]string{keySource: "https://github.com/a/b"}, "digest", "https://github.com/a/b/releases"},
		{map[string]string{keySource: "git+https://github.com/a/b.git/tree/main"}, "version", "https://github.com/a/b/releases/tag/1.2"},
		{map[string]string{keySource: "https://gitlab.com/a/b", keyURL: "https://example.org"}, "version", "https://example.org"},
		{map[string]string{keyDocumentation: "https://docs.example.org"}, "digest", "https://docs.example.org"},
		{map[string]string{keyURL: "javascript:alert(1)"}, "digest", ""},
		{nil, "digest", ""},
	}
	for _, c := range cases {
		if got := ReleaseURL(c.meta, c.kind, "1.2"); got != c.want {
			t.Errorf("ReleaseURL(%v) = %q, want %q", c.meta, got, c.want)
		}
	}
}

func TestReleaseCandidatesAndLinkExists(t *testing.T) {
	meta := map[string]string{keySource: "https://github.com/nginx/docker-nginx.git#a16f1329e1:mainline"}
	got := ReleaseCandidates(meta, "version", "1.31-alpine")
	if strings.Join(got, " ") != "https://github.com/nginx/docker-nginx/releases/tag/1.31-alpine https://github.com/nginx/docker-nginx/releases" {
		t.Fatalf("candidates = %v", got)
	}
	if got := ReleaseCandidates(meta, "digest", ""); len(got) != 1 || got[0] != "https://github.com/nginx/docker-nginx/releases" {
		t.Fatalf("digest candidates = %v", got)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/ok" {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	if !LinkExists(context.Background(), srv.Client(), srv.URL+"/ok") || LinkExists(context.Background(), srv.Client(), srv.URL+"/missing") {
		t.Fatal("LinkExists")
	}
}

func TestGHCRTokenServesOnlyGHCRHosts(t *testing.T) {
	// Other registries fall through to the default keychain, so hide any
	// credentials of the machine running the test (GitHub's runners log in
	// to Docker Hub in ~/.docker/config.json).
	empty := t.TempDir()
	for _, v := range []string{"HOME", "USERPROFILE", "DOCKER_CONFIG", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "REGISTRY_AUTH_FILE"} {
		t.Setenv(v, empty)
	}
	c := New(Config{GHCRToken: "ghp_x"})
	for host, want := range map[string]string{"ghcr.io": "ghp_x", "lscr.io": "ghp_x", "index.docker.io": ""} {
		reg, _ := name.NewRegistry(host)
		a, err := c.keychain.Resolve(reg)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := a.Authorization()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Password != want {
			t.Errorf("%s: password %q, want %q", host, cfg.Password, want)
		}
	}
	if New(Config{}).keychain != authn.DefaultKeychain {
		t.Error("without GHCR_TOKEN the default keychain must be used")
	}
}
