// Package registry is the bot's OCI registry client: digest
// checks with HEAD, tag listing, rate-limit handling, and release-link
// discovery. Core detection needs no per-registry code.
package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

var (
	// ErrRateLimited means the registry answered 429, or is still inside the
	// Retry-After window of an earlier 429.
	ErrRateLimited = errors.New("rate limited by registry")
	// ErrNotFound means the repository or tag does not exist.
	ErrNotFound = errors.New("not found in registry")
)

// RateLimitError is ErrRateLimited with the time the registry host may be
// asked again.
type RateLimitError struct {
	Host       string
	Until      time.Time
	RetryAfter time.Duration // set on the request that received the 429
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("%v: %s (retry after %s)", ErrRateLimited, e.Host, e.RetryAfter)
	}
	return fmt.Sprintf("%v: %s until %s", ErrRateLimited, e.Host, e.Until.UTC().Format(time.RFC3339))
}

func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// defaultRetryAfter is used when a 429 carries no usable Retry-After.
const defaultRetryAfter = 5 * time.Minute

// defaultRequestGap is the pause between two HTTP requests to one registry
// host when Config.RequestGap is zero.
const defaultRequestGap = 500 * time.Millisecond

// Config configures a Client.
type Config struct {
	ConcurrencyPerHost int               // REGISTRY_CONCURRENCY_PER_HOST
	RequestGap         time.Duration     // minimum spacing of requests per host; 0: 500 ms
	GHCRToken          string            // GHCR_TOKEN: GitHub token for ghcr.io and lscr.io
	PushOrderedHosts   []string          // registries that list tags in push order; nil: GHCRHosts
	Transport          http.RoundTripper // nil: remote.DefaultTransport
	Insecure           bool              // plain HTTP registries (tests)
	UserAgent          string
	Now                func() time.Time
}

// Client talks to OCI registries. It is safe for concurrent use.
type Client struct {
	cfg      Config
	pace     *pacer
	keychain authn.Keychain

	mu      sync.Mutex
	sems    map[string]chan struct{}
	limited map[string]time.Time // registry host → retry not before
	auths   map[string]*hostAuth // registry host → shared transport
}

// New returns a client that authenticates with the default keychain
// (anonymous unless DOCKER_CONFIG/config.json has credentials).
func New(cfg Config) *Client {
	if cfg.ConcurrencyPerHost < 1 {
		cfg.ConcurrencyPerHost = 4
	}
	if cfg.Transport == nil {
		cfg.Transport = remote.DefaultTransport
	}
	if cfg.RequestGap <= 0 {
		cfg.RequestGap = defaultRequestGap
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PushOrderedHosts == nil {
		cfg.PushOrderedHosts = GHCRHosts
	}
	pace := &pacer{next: cfg.Transport, gap: cfg.RequestGap, at: map[string]time.Time{}, sent: map[string]int{}}
	var keychain authn.Keychain = authn.DefaultKeychain
	if cfg.GHCRToken != "" {
		keychain = authn.NewMultiKeychain(ghcrKeychain{token: cfg.GHCRToken}, authn.DefaultKeychain)
	}
	return &Client{cfg: cfg, pace: pace, keychain: keychain, sems: map[string]chan struct{}{},
		limited: map[string]time.Time{}, auths: map[string]*hostAuth{}}
}

// GHCRHosts are the registries GHCR_TOKEN logs in to. lscr.io serves
// LinuxServer images with ghcr.io tokens, so the same login works there.
var GHCRHosts = []string{"ghcr.io", "lscr.io"}

// ghcrKeychain answers GHCR_TOKEN for GHCRHosts and anonymous otherwise, so
// the default keychain (DOCKER_CONFIG) still serves every other registry.
// ghcr.io checks only the token; the user name is not used.
type ghcrKeychain struct{ token string }

func (k ghcrKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	for _, h := range GHCRHosts {
		if r.RegistryStr() == h {
			return &authn.Basic{Username: "ddum", Password: k.token}, nil
		}
	}
	return authn.Anonymous, nil
}

// Digest returns the manifest digest of repo:tag using HEAD, which Docker Hub
// does not count against its pull limit. For a multi-arch
// image this is the index digest, which is what RepoDigests holds.
func (c *Client) Digest(ctx context.Context, repo, tag string) (string, error) {
	ref, err := name.NewTag(repo+":"+tag, c.nameOpts()...)
	if err != nil {
		return "", fmt.Errorf("parse %s:%s: %w", repo, tag, err)
	}
	var out string
	err = c.do(ctx, ref.Context(), func(ctx context.Context, rt http.RoundTripper) error {
		d, err := remote.Head(ref, remoteOpts(ctx, rt)...)
		if err != nil {
			return err
		}
		out = d.Digest.String()
		return nil
	})
	return out, err
}

// ManifestExists reports whether repo holds the manifest digest. It answers
// false only when the registry says so; other failures are errors.
func (c *Client) ManifestExists(ctx context.Context, repo, digest string) (bool, error) {
	ref, err := name.NewDigest(repo+"@"+digest, c.nameOpts()...)
	if err != nil {
		return false, fmt.Errorf("parse %s@%s: %w", repo, digest, err)
	}
	err = c.do(ctx, ref.Context(), func(ctx context.Context, rt http.RoundTripper) error {
		_, err := remote.Head(ref, remoteOpts(ctx, rt)...)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Platforms lists the platforms the image at repo@digest is built for, as
// os/arch or os/arch/variant. A manifest GET counts against Docker Hub's pull
// limit, so callers keep the answer: a digest's platforms never change.
func (c *Client) Platforms(ctx context.Context, repo, digest string) ([]string, error) {
	ref, err := name.NewDigest(repo+"@"+digest, c.nameOpts()...)
	if err != nil {
		return nil, fmt.Errorf("parse %s@%s: %w", repo, digest, err)
	}
	var out []string
	err = c.do(ctx, ref.Context(), func(ctx context.Context, rt http.RoundTripper) error {
		desc, err := remote.Get(ref, remoteOpts(ctx, rt)...)
		if err != nil {
			return err
		}
		if desc.MediaType.IsIndex() {
			idx, err := desc.ImageIndex()
			if err != nil {
				return err
			}
			m, err := idx.IndexManifest()
			if err != nil {
				return err
			}
			for _, d := range m.Manifests {
				// Build attestations are listed as unknown/unknown.
				if d.Platform != nil && d.Platform.OS != "unknown" && d.Platform.OS != "" {
					out = append(out, platformString(d.Platform.OS, d.Platform.Architecture, d.Platform.Variant))
				}
			}
			return nil
		}
		img, err := desc.Image()
		if err != nil {
			return err
		}
		cf, err := img.ConfigFile()
		if err != nil {
			return err
		}
		out = append(out, platformString(cf.OS, cf.Architecture, cf.Variant))
		return nil
	})
	return out, err
}

func platformString(os, arch, variant string) string {
	if variant != "" {
		return os + "/" + arch + "/" + variant
	}
	return os + "/" + arch
}

// IsMissing reports a registry saying it has no such image, or that it will not
// say: registries answer an unknown repository with an authorization error, so
// a repository that does not exist and a private one look the same.
func IsMissing(err error) bool {
	if errors.Is(err, ErrNotFound) {
		return true
	}
	var te *transport.Error
	return errors.As(err, &te) && (te.StatusCode == http.StatusUnauthorized || te.StatusCode == http.StatusForbidden)
}

// Tags lists every tag of repo (all pages).
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error) {
	r, err := name.NewRepository(repo, c.nameOpts()...)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", repo, err)
	}
	var out []string
	err = c.do(ctx, r, func(ctx context.Context, rt http.RoundTripper) error {
		tags, err := remote.List(r, remoteOpts(ctx, rt)...)
		out = tags
		return err
	})
	return out, err
}

// PushOrdered reports whether repo's registry lists tags in push order,
// oldest first, so TagsAfter returns the tags pushed since a known one.
// ghcr.io (and lscr.io, which serves it) do; most registries list lexically.
func (c *Client) PushOrdered(repo string) bool {
	r, err := name.NewRepository(repo, c.nameOpts()...)
	return err == nil && slices.Contains(c.cfg.PushOrderedHosts, r.RegistryStr())
}

// TagsAfter lists the tags of repo that come after the tag last (all pages),
// using the distribution API's last parameter, which go-containerregistry
// does not expose. ghcr.io answers no tags when last does not exist.
func (c *Client) TagsAfter(ctx context.Context, repo, last string) ([]string, error) {
	r, err := name.NewRepository(repo, c.nameOpts()...)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", repo, err)
	}
	u := url.URL{Scheme: r.Scheme(), Host: r.RegistryStr(), Path: "/v2/" + r.RepositoryStr() + "/tags/list",
		RawQuery: url.Values{"n": {"1000"}, "last": {last}}.Encode()}
	var out []string
	err = c.do(ctx, r, func(ctx context.Context, rt http.RoundTripper) error {
		client := &http.Client{Transport: rt}
		for next := u.String(); next != ""; {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
			if err != nil {
				return err
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			if err := transport.CheckError(resp, http.StatusOK); err != nil {
				resp.Body.Close()
				return err
			}
			var page struct {
				Tags []string `json:"tags"`
			}
			err = json.NewDecoder(resp.Body).Decode(&page)
			resp.Body.Close()
			if err != nil {
				return fmt.Errorf("decode tag list: %w", err)
			}
			out = append(out, page.Tags...)
			if next, err = nextPage(req.URL, resp.Header.Get("Link")); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// nextPage resolves the rel="next" URL of a Link header against base, or
// returns "" when there is none. Pages must stay on base's host.
func nextPage(base *url.URL, link string) (string, error) {
	start, end := strings.Index(link, "<"), strings.Index(link, ">")
	if start < 0 || end < start || !strings.Contains(link[end:], `rel="next"`) {
		return "", nil
	}
	u, err := base.Parse(link[start+1 : end])
	if err != nil {
		return "", fmt.Errorf("tag list Link header: %w", err)
	}
	if u.Host != base.Host {
		return "", fmt.Errorf("tag list page on another host: %s", u.Host)
	}
	return u.String(), nil
}

// Metadata returns the OCI annotations of the manifest (or index) of
// repo:tag merged with the labels of its linux/amd64 image config. This is
// several GET requests, so callers fetch it once per new update.
func (c *Client) Metadata(ctx context.Context, repo, tag string) (map[string]string, error) {
	ref, err := name.NewTag(repo+":"+tag, c.nameOpts()...)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	err = c.do(ctx, ref.Context(), func(ctx context.Context, rt http.RoundTripper) error {
		// The descriptor's Image reuses the manifest and the auth already
		// fetched here, where remote.Image would repeat both. The default
		// platform is linux/amd64.
		desc, err := remote.Get(ref, remoteOpts(ctx, rt)...)
		if err != nil {
			return err
		}
		var m struct {
			Annotations map[string]string `json:"annotations"`
		}
		if json.Unmarshal(desc.Manifest, &m) == nil {
			for k, v := range m.Annotations {
				out[k] = v
			}
		}
		img, err := desc.Image()
		if err != nil {
			return nil // annotations alone are fine
		}
		if mf, err := img.Manifest(); err == nil {
			for k, v := range mf.Annotations {
				if _, ok := out[k]; !ok {
					out[k] = v
				}
			}
		}
		if cf, err := img.ConfigFile(); err == nil {
			for k, v := range cf.Config.Labels {
				if _, ok := out[k]; !ok {
					out[k] = v
				}
			}
		}
		return nil
	})
	return out, err
}

func (c *Client) nameOpts() []name.Option {
	if c.cfg.Insecure {
		return []name.Option{name.Insecure}
	}
	return nil
}

// remoteOpts makes remote use rt as is, without its own ping and token
// exchange, so a call costs only its own requests.
func remoteOpts(ctx context.Context, rt http.RoundTripper) []remote.Option {
	return []remote.Option{remote.WithContext(ctx), remote.WithTransport(rt)}
}

// do runs fn with the per-registry concurrency limit and rate-limit window,
// over the registry host's shared authenticated transport.
func (c *Client) do(ctx context.Context, repo name.Repository, fn func(context.Context, http.RoundTripper) error) error {
	host := repo.RegistryStr()
	c.mu.Lock()
	until := c.limited[host]
	sem, ok := c.sems[host]
	if !ok {
		sem = make(chan struct{}, c.cfg.ConcurrencyPerHost)
		c.sems[host] = sem
	}
	c.mu.Unlock()
	if c.cfg.Now().Before(until) {
		return &RateLimitError{Host: host, Until: until}
	}

	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-sem }()

	box := &retryAfterBox{}
	ctx = context.WithValue(ctx, retryAfterKey{}, box)
	rt, err := c.transport(ctx, repo)
	if err == nil {
		err = fn(ctx, rt)
	}
	if err == nil {
		return nil
	}
	retryAfter := box.get()
	var te *transport.Error
	switch {
	case retryAfter > 0 || (errors.As(err, &te) && te.StatusCode == http.StatusTooManyRequests):
		wait := retryAfter
		if wait <= 0 {
			wait = defaultRetryAfter
		}
		until := c.cfg.Now().Add(wait)
		c.mu.Lock()
		c.limited[host] = until
		c.mu.Unlock()
		return &RateLimitError{Host: host, Until: until, RetryAfter: wait}
	case errors.As(err, &te) && isNotFound(te):
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return err
}

// authMaxAge is how long a host's shared transport, with its token and the
// scopes it has gathered, is reused before it is rebuilt from the keychain.
const authMaxAge = 12 * time.Hour

// hostAuth is the authenticated transport shared by every request to one
// registry host. Building it costs a ping and a token request; after that the
// bearer transport refreshes its token on a 401 and adds any scope a
// challenge names, so a lookup costs only its own request (an anonymous
// ghcr.io token also reads every public repository, and lscr.io issues
// ghcr.io tokens).
type hostAuth struct {
	mu    sync.Mutex
	rt    http.RoundTripper
	built time.Time
}

// transport returns the shared transport of repo's registry host, building it
// when it is missing or older than authMaxAge.
func (c *Client) transport(ctx context.Context, repo name.Repository) (http.RoundTripper, error) {
	host := repo.RegistryStr()
	c.mu.Lock()
	a, ok := c.auths[host]
	if !ok {
		a = &hostAuth{}
		c.auths[host] = a
	}
	c.mu.Unlock()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rt != nil && c.cfg.Now().Sub(a.built) < authMaxAge {
		return a.rt, nil
	}
	auth, err := authn.Resolve(ctx, c.keychain, repo.Registry)
	if err != nil {
		return nil, err
	}
	// remote adds no retries or user agent to a prepared transport, so they
	// are added here. 429 is not retried: the whole registry is skipped
	// until Retry-After instead.
	var inner http.RoundTripper = &recorder{next: c.pace}
	inner = transport.NewRetry(inner, transport.WithRetryStatusCodes(http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout))
	inner = transport.NewUserAgent(inner, c.cfg.UserAgent)
	rt, err := transport.NewWithContext(ctx, repo.Registry, auth, inner, []string{repo.Scope(transport.PullScope)})
	if err != nil {
		return nil, err
	}
	a.rt, a.built = rt, c.cfg.Now()
	return rt, nil
}

func isNotFound(te *transport.Error) bool {
	if te.StatusCode == http.StatusNotFound {
		return true
	}
	for _, d := range te.Errors {
		switch d.Code {
		case transport.ManifestUnknownErrorCode, transport.NameUnknownErrorCode:
			return true
		}
	}
	return false
}

// retryAfterKey is the context key of the retryAfterBox of one call.
type retryAfterKey struct{}

// retryAfterBox carries a 429's Retry-After from the shared transport back to
// the call whose request (or token exchange) received it.
type retryAfterBox struct {
	mu sync.Mutex
	d  time.Duration
}

func (b *retryAfterBox) get() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.d
}

// recorder captures Retry-After from 429 responses, which transport.Error
// does not expose, into the request's retryAfterBox.
type recorder struct {
	next http.RoundTripper
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.next.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		d := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		if d <= 0 {
			d = defaultRetryAfter
		}
		if box, ok := req.Context().Value(retryAfterKey{}).(*retryAfterBox); ok {
			box.mu.Lock()
			box.d = d
			box.mu.Unlock()
		}
	}
	return resp, err
}

// pacer spaces the HTTP requests to each registry host at least gap apart,
// token and ping requests included, so a check cycle trickles requests out
// instead of bursting them and tripping the registry's short-window limits.
type pacer struct {
	next http.RoundTripper
	gap  time.Duration

	mu   sync.Mutex
	at   map[string]time.Time // host → earliest start of its next request
	sent map[string]int       // host → requests sent since the last take
}

func (p *pacer) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := p.wait(req.Context(), req.URL.Host); err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.sent[req.URL.Host]++
	p.mu.Unlock()
	return p.next.RoundTrip(req)
}

// TakeRequests returns the number of HTTP requests sent to each host, token
// and ping requests included, since the previous call, and starts counting
// afresh.
func (c *Client) TakeRequests() map[string]int {
	c.pace.mu.Lock()
	defer c.pace.mu.Unlock()
	out := c.pace.sent
	c.pace.sent = map[string]int{}
	return out
}

// wait reserves the next slot for host and sleeps until it arrives.
func (p *pacer) wait(ctx context.Context, host string) error {
	p.mu.Lock()
	start := time.Now()
	if t := p.at[host]; t.After(start) {
		start = t
	}
	p.at[host] = start.Add(p.gap)
	p.mu.Unlock()

	d := time.Until(start)
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if s, err := strconv.Atoi(v); err == nil {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return t.Sub(now)
	}
	return 0
}

// OCI annotation and label keys used for release links.
const (
	keySource        = "org.opencontainers.image.source"
	keyURL           = "org.opencontainers.image.url"
	keyDocumentation = "org.opencontainers.image.documentation"
)

// ReleaseCandidates returns the release links to try for an update, best
// first: for a GitHub source and a version update, the tag's release page and
// then the releases list; otherwise the single ReleaseURL.
func ReleaseCandidates(meta map[string]string, kind, tag string) []string {
	if repo := githubRepo(meta[keySource]); repo != "" && kind == "version" && tag != "" {
		return []string{repo + "/releases/tag/" + tag, repo + "/releases"}
	}
	if u := ReleaseURL(meta, kind, tag); u != "" {
		return []string{u}
	}
	return nil
}

// LinkExists reports whether url answers a HEAD request (after redirects)
// with 2xx. It is used once per new update, never per check.
func LinkExists(ctx context.Context, client *http.Client, url string) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// ReleaseURL builds the release link for an update from image metadata
// : GitHub releases when the source is a GitHub repository,
// otherwise the image URL or documentation, otherwise nothing.
func ReleaseURL(meta map[string]string, kind, tag string) string {
	if repo := githubRepo(meta[keySource]); repo != "" {
		if kind == "version" && tag != "" {
			return repo + "/releases/tag/" + tag
		}
		return repo + "/releases"
	}
	for _, k := range []string{keyURL, keyDocumentation} {
		if u := meta[k]; strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
			return u
		}
	}
	return ""
}

// githubRepo normalizes a source URL to https://github.com/<owner>/<repo>,
// or returns "" if it is not a GitHub repository.
func githubRepo(src string) string {
	src = strings.TrimSpace(strings.TrimPrefix(src, "git+"))
	// Drop a "#commit:dir" or "?query" suffix (docker-library images use one).
	if i := strings.IndexAny(src, "#?"); i >= 0 {
		src = src[:i]
	}
	for _, p := range []string{"https://", "http://", "git://"} {
		src = strings.TrimPrefix(src, p)
	}
	src = strings.TrimPrefix(src, "www.")
	if !strings.HasPrefix(src, "github.com/") {
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(src, "github.com/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ""
	}
	return "https://github.com/" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
}
