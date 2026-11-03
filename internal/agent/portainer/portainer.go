// Package portainer is the agent's client for the Portainer API . Endpoints and payloads were checked against the Portainer 2.45.1
// source (api/http/handler/stacks).
package portainer

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Stack types (portainer.StackType).
const (
	TypeSwarm   = 1
	TypeCompose = 2
)

// StatusInactive is the Status of a stopped stack (portainer.StackStatus).
const StatusInactive = 2

// Pair is one entry of a stack's Env list.
type Pair struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// GitConfig is the part of a Git-based stack's repository settings we use.
type GitConfig struct {
	URL            string `json:"URL"`
	ReferenceName  string `json:"ReferenceName"`
	ConfigFilePath string `json:"ConfigFilePath"`
}

// Stack is a Portainer stack.
type Stack struct {
	ID          int        `json:"Id"`
	Name        string     `json:"Name"`
	Type        int        `json:"Type"`
	EndpointID  int        `json:"EndpointId"`
	EntryPoint  string     `json:"EntryPoint"`
	Env         []Pair     `json:"Env"`
	Status      int        `json:"Status"`
	ProjectPath string     `json:"ProjectPath"`
	GitConfig   *GitConfig `json:"GitConfig"`
}

// GitBased reports whether the stack is deployed from a Git repository.
func (s Stack) GitBased() bool { return s.GitConfig != nil && s.GitConfig.URL != "" }

// Client talks to one Portainer instance with an API access token.
type Client struct {
	BaseURL    string // e.g. https://portainer:9443
	Token      string
	EndpointID int
	HTTP       *http.Client
}

// New returns a client. caFile (PORTAINER_CA_FILE) names a PEM file of
// certificates to trust, which is how to use Portainer's self-signed certificate
// safely. insecureTLS (PORTAINER_INSECURE_TLS) accepts any certificate, so the
// token goes to whoever answers.
func New(baseURL, token string, endpointID int, insecureTLS bool, caFile string) (*Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	switch {
	case insecureTLS:
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in
	case caFile != "":
		pemData, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("PORTAINER_CA_FILE: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("PORTAINER_CA_FILE: no certificates in %s", caFile)
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, EndpointID: endpointID,
		HTTP: &http.Client{Timeout: 5 * time.Minute, Transport: tr}}, nil
}

// Error is a non-2xx answer from Portainer.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("portainer: HTTP %d: %s", e.Status, e.Message) }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := c.BaseURL + "/api" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-KEY", c.Token) // never logged
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
			Details string `json:"details"`
		}
		json.Unmarshal(data, &e)
		msg := strings.TrimSpace(e.Message + " " + e.Details)
		if msg == "" {
			msg = strings.TrimSpace(string(data))
		}
		return &Error{Status: resp.StatusCode, Message: msg}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Stacks lists the Compose stacks of the configured environment.
func (c *Client) Stacks(ctx context.Context) ([]Stack, error) {
	f, _ := json.Marshal(map[string]int{"EndpointID": c.EndpointID})
	var all []Stack
	if err := c.do(ctx, http.MethodGet, "/stacks", url.Values{"filters": {string(f)}}, nil, &all); err != nil {
		return nil, err
	}
	var out []Stack
	for _, s := range all {
		if s.Type == TypeCompose && s.EndpointID == c.EndpointID {
			out = append(out, s)
		}
	}
	return out, nil
}

// Stack returns one stack.
func (c *Client) Stack(ctx context.Context, id int) (Stack, error) {
	var s Stack
	err := c.do(ctx, http.MethodGet, "/stacks/"+strconv.Itoa(id), nil, nil, &s)
	return s, err
}

// StackFile returns the stack's Compose file content.
func (c *Client) StackFile(ctx context.Context, id int) (string, error) {
	var r struct {
		StackFileContent string `json:"StackFileContent"`
	}
	err := c.do(ctx, http.MethodGet, "/stacks/"+strconv.Itoa(id)+"/file", nil, nil, &r)
	return r.StackFileContent, err
}

// UpdateFileStack redeploys a file-based stack with new content and the
// complete Env list (omitting entries would drop them), never pulling:
// images were pre-pulled by digest and tagged locally.
func (c *Client) UpdateFileStack(ctx context.Context, id int, content string, env []Pair) error {
	if env == nil {
		env = []Pair{}
	}
	body := map[string]any{"StackFileContent": content, "Env": env, "Prune": false, "RepullImageAndRedeploy": false}
	q := url.Values{"endpointId": {strconv.Itoa(c.EndpointID)}}
	return c.do(ctx, http.MethodPut, "/stacks/"+strconv.Itoa(id), q, body, nil)
}

// RedeployGitStack redeploys a Git-based stack from its current reference
// without pulling images (same-tag digest updates only).
func (c *Client) RedeployGitStack(ctx context.Context, s Stack) error {
	ref := ""
	if s.GitConfig != nil {
		ref = s.GitConfig.ReferenceName
	}
	env := s.Env
	if env == nil {
		env = []Pair{}
	}
	body := map[string]any{"RepositoryReferenceName": ref, "Env": env, "RepullImageAndRedeploy": false, "StackName": s.Name}
	q := url.Values{"endpointId": {strconv.Itoa(c.EndpointID)}}
	return c.do(ctx, http.MethodPut, "/stacks/"+strconv.Itoa(s.ID)+"/git/redeploy", q, body, nil)
}
