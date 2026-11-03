package protocol

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

var (
	testKey   = []byte("0123456789abcdef0123456789abcdef")
	otherKey  = []byte("fedcba9876543210fedcba9876543210")
	testAgent = "0195f2c4-0000-7000-8000-000000000001"
	testNow   = time.Date(2026, 10, 1, 2, 15, 0, 0, time.UTC)
	testBody  = []byte(`{"protocol_version":1}`)
)

func signed(t *testing.T) (ts string, sig string) {
	t.Helper()
	sig = SignRequest(testKey, "POST", "/v1/agent/poll", testNow.Unix(), testAgent, testBody)
	return strconv.FormatInt(testNow.Unix(), 10), sig
}

func TestVerifyRequestRoundTrip(t *testing.T) {
	ts, sig := signed(t)
	if err := VerifyRequest(testKey, "POST", "/v1/agent/poll", testAgent, ts, sig, testBody, testNow); err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
}

func TestVerifyRequestTampered(t *testing.T) {
	ts, sig := signed(t)
	cases := []struct {
		name                  string
		key                   []byte
		method, path, agentID string
		body                  []byte
	}{
		{"body", testKey, "POST", "/v1/agent/poll", testAgent, []byte(`{"protocol_version":2}`)},
		{"method", testKey, "PUT", "/v1/agent/poll", testAgent, testBody},
		{"path", testKey, "POST", "/v1/agent/inventory", testAgent, testBody},
		{"query", testKey, "POST", "/v1/agent/poll?x=1", testAgent, testBody},
		{"agent", testKey, "POST", "/v1/agent/poll", "0195f2c4-0000-7000-8000-000000000002", testBody},
		{"key", otherKey, "POST", "/v1/agent/poll", testAgent, testBody},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := VerifyRequest(c.key, c.method, c.path, c.agentID, ts, sig, c.body, testNow)
			if !errors.Is(err, ErrBadSignature) {
				t.Fatalf("got %v, want ErrBadSignature", err)
			}
		})
	}
}

func TestVerifyRequestTamperedTimestamp(t *testing.T) {
	_, sig := signed(t)
	ts := strconv.FormatInt(testNow.Unix()+1, 10)
	if err := VerifyRequest(testKey, "POST", "/v1/agent/poll", testAgent, ts, sig, testBody, testNow); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("got %v, want ErrBadSignature", err)
	}
}

func TestVerifyRequestClockSkew(t *testing.T) {
	cases := []struct {
		name    string
		offset  time.Duration // request time relative to server time
		wantErr bool
	}{
		{"exact", 0, false},
		{"ahead at limit", MaxClockSkew, false},
		{"behind at limit", -MaxClockSkew, false},
		{"ahead past limit", MaxClockSkew + time.Second, true},
		{"behind past limit", -MaxClockSkew - time.Second, true},
		{"far behind", -2 * time.Hour, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reqTime := testNow.Add(c.offset)
			sig := SignRequest(testKey, "POST", "/v1/agent/poll", reqTime.Unix(), testAgent, testBody)
			err := VerifyRequest(testKey, "POST", "/v1/agent/poll", testAgent,
				strconv.FormatInt(reqTime.Unix(), 10), sig, testBody, testNow)
			if !c.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var skew *ClockSkewError
			if !errors.As(err, &skew) {
				t.Fatalf("got %v, want *ClockSkewError", err)
			}
			if skew.Skew != c.offset {
				t.Fatalf("skew = %s, want %s", skew.Skew, c.offset)
			}
		})
	}
}

// A request with a bad signature and a bad clock must report the signature,
// so the bot only posts clock-skew notices for genuine agents.
func TestVerifyRequestSignatureCheckedBeforeTimestamp(t *testing.T) {
	old := testNow.Add(-time.Hour)
	sig := SignRequest(otherKey, "POST", "/v1/agent/poll", old.Unix(), testAgent, testBody)
	err := VerifyRequest(testKey, "POST", "/v1/agent/poll", testAgent,
		strconv.FormatInt(old.Unix(), 10), sig, testBody, testNow)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("got %v, want ErrBadSignature", err)
	}
}

func TestVerifyRequestMalformed(t *testing.T) {
	ts, sig := signed(t)
	cases := []struct {
		name          string
		agent, ts, sg string
		want          error
	}{
		{"no agent", "", ts, sig, ErrMissingHeaders},
		{"no timestamp", testAgent, "", sig, ErrMissingHeaders},
		{"no signature", testAgent, ts, "", ErrMissingHeaders},
		{"non-numeric timestamp", testAgent, "yesterday", sig, ErrBadTimestamp},
		{"non-hex signature", testAgent, ts, "zz" + sig[2:], ErrBadSignature},
		{"truncated signature", testAgent, ts, sig[:10], ErrBadSignature},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := VerifyRequest(testKey, "POST", "/v1/agent/poll", c.agent, c.ts, c.sg, testBody, testNow)
			if !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
		})
	}
}

func TestVerifyRequestUppercaseSignatureAccepted(t *testing.T) {
	ts, sig := signed(t)
	upper := []byte(sig)
	for i, c := range upper {
		if c >= 'a' && c <= 'f' {
			upper[i] = c - 'a' + 'A'
		}
	}
	if err := VerifyRequest(testKey, "POST", "/v1/agent/poll", testAgent, ts, string(upper), testBody, testNow); err != nil {
		t.Fatalf("uppercase hex rejected: %v", err)
	}
}

func TestVerifyResponse(t *testing.T) {
	_, reqSig := signed(t)
	respBody := []byte(`{"commands":[]}`)
	respSig := SignResponse(testKey, reqSig, testNow.Unix(), respBody)
	ts := strconv.FormatInt(testNow.Unix(), 10)

	if err := VerifyResponse(testKey, reqSig, ts, respSig, respBody); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	if err := VerifyResponse(testKey, reqSig, ts, respSig, []byte(`{"commands":[{}]}`)); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered body: got %v, want ErrBadSignature", err)
	}
	if err := VerifyResponse(otherKey, reqSig, ts, respSig, respBody); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key: got %v, want ErrBadSignature", err)
	}
	if err := VerifyResponse(testKey, reqSig, "", respSig, respBody); !errors.Is(err, ErrMissingHeaders) {
		t.Fatalf("missing timestamp: got %v, want ErrMissingHeaders", err)
	}
}

// An old response replayed to a new request must not verify, because the
// response signature is bound to the request signature.
func TestVerifyResponseReplayRejected(t *testing.T) {
	_, firstReqSig := signed(t)
	respBody := []byte(`{"commands":[{"operation_id":"x"}]}`)
	respSig := SignResponse(testKey, firstReqSig, testNow.Unix(), respBody)
	ts := strconv.FormatInt(testNow.Unix(), 10)

	later := testNow.Add(30 * time.Second)
	secondReqSig := SignRequest(testKey, "POST", "/v1/agent/poll", later.Unix(), testAgent, testBody)

	if err := VerifyResponse(testKey, secondReqSig, ts, respSig, respBody); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("replayed response: got %v, want ErrBadSignature", err)
	}
}

// End to end over real HTTP: the client signs with URL.RequestURI(), the
// server verifies with RequestURI, including a query string with escapes.
func TestHTTPRoundTrip(t *testing.T) {
	respBody := []byte(`{"ok":true}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := VerifyHTTPRequest(r, testKey, body, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		SignHTTPResponse(w.Header(), testKey, r.Header.Get(HeaderSignature), respBody, time.Now())
		w.Write(respBody)
	}))
	defer srv.Close()

	req, err := http.NewRequest("POST", srv.URL+"/v1/agent/poll?name=a%20b&x=1", bytes.NewReader(testBody))
	if err != nil {
		t.Fatal(err)
	}
	reqSig := SignHTTPRequest(req, testKey, testAgent, testBody, time.Now())
	if got := req.Header.Get(HeaderProtocol); got != "1" {
		t.Fatalf("protocol header = %q", got)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if err := VerifyHTTPResponse(resp, testKey, reqSig, body); err != nil {
		t.Fatalf("response rejected: %v", err)
	}
}

func TestSupportsVersion(t *testing.T) {
	if !SupportsVersion(Version) {
		t.Fatal("current version not supported")
	}
	if SupportsVersion(0) || SupportsVersion(MaxSupportedVersion+1) {
		t.Fatal("out-of-range version accepted")
	}
}
