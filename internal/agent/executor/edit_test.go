package executor

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// scalarPos finds the image: scalar of a service the way discovery does.
func scalarPos(t *testing.T, data string, service string) (int, int, string) {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatal(err)
	}
	svcs := doc.Content[0].Content
	for i := 0; i < len(svcs); i += 2 {
		if svcs[i].Value != "services" {
			continue
		}
		m := svcs[i+1].Content
		for j := 0; j < len(m); j += 2 {
			if m[j].Value == service {
				s := m[j+1].Content
				for k := 0; k < len(s); k += 2 {
					if s[k].Value == "image" {
						return s[k+1].Line, s[k+1].Column, s[k+1].Value
					}
				}
			}
		}
	}
	t.Fatalf("service %s not found", service)
	return 0, 0, ""
}

func TestReplaceYAMLScalarKeepsFormatting(t *testing.T) {
	in := "# my stack\nservices:\n  web:\n    image: nginx:1.27-alpine   # pinned on purpose\n    restart: always\n" +
		"  api:\n    image: \"ghcr.io/x/api:1.0\"\n  ünï:\n    image: 'redis:7.2'\n"
	cases := []struct {
		svc, newValue, want string
	}{
		{"web", "nginx:1.31-alpine", "# my stack\nservices:\n  web:\n    image: nginx:1.31-alpine   # pinned on purpose\n    restart: always\n" +
			"  api:\n    image: \"ghcr.io/x/api:1.0\"\n  ünï:\n    image: 'redis:7.2'\n"},
		{"api", "ghcr.io/x/api:1.1", "# my stack\nservices:\n  web:\n    image: nginx:1.27-alpine   # pinned on purpose\n    restart: always\n" +
			"  api:\n    image: \"ghcr.io/x/api:1.1\"\n  ünï:\n    image: 'redis:7.2'\n"},
		{"ünï", "redis:7.4", "# my stack\nservices:\n  web:\n    image: nginx:1.27-alpine   # pinned on purpose\n    restart: always\n" +
			"  api:\n    image: \"ghcr.io/x/api:1.0\"\n  ünï:\n    image: 'redis:7.4'\n"},
	}
	for _, c := range cases {
		line, col, old := scalarPos(t, in, c.svc)
		out, err := ReplaceYAMLScalar([]byte(in), line, col, old, c.newValue)
		if err != nil {
			t.Fatalf("%s: %v", c.svc, err)
		}
		if string(out) != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.svc, out, c.want)
		}
	}
	if _, err := ReplaceYAMLScalar([]byte(in), 4, 12, "nginx:9", "x"); err == nil {
		t.Error("a changed file must be refused")
	}
}

func TestReplaceEnvValue(t *testing.T) {
	cases := []struct{ in, key, val, want string }{
		{"# tags\nIMMICH_VERSION=v1.140.1\nOTHER=x\n", "IMMICH_VERSION", "v1.141.0", "# tags\nIMMICH_VERSION=v1.141.0\nOTHER=x\n"},
		{"TAG=\"1.0\"\r\nX=1\r\n", "TAG", "1.1", "TAG=\"1.1\"\r\nX=1\r\n"},
		{"export TAG='1.0' # keep\n", "TAG", "1.1", "export TAG='1.1' # keep\n"},
		{"TAG=1.0 # note\nTAG=2.0", "TAG", "3.0", "TAG=1.0 # note\nTAG=3.0"},
		{"TAG = 1.0\n", "TAG", "1.1", "TAG = 1.1\n"},
	}
	for _, c := range cases {
		out, err := ReplaceEnvValue([]byte(c.in), c.key, c.val)
		if err != nil || string(out) != c.want {
			t.Errorf("ReplaceEnvValue(%q) = %q, %v; want %q", c.in, out, err, c.want)
		}
	}
	if _, err := ReplaceEnvValue([]byte("A=1\n"), "TAG", "1"); err == nil {
		t.Error("missing key must fail")
	}
	if _, err := ReplaceEnvValue([]byte("TAG=1\n"), "TAG", "1; rm -rf /"); err == nil {
		t.Error("odd values must be refused")
	}
}
