package discovery

import (
	"bufio"
	"bytes"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"

	"github.com/DragonDrew/ddum/internal/protocol"
)

// Tag-change sources and reasons.
const (
	TagSourceImage   = "image"
	TagSourceEnvFile = "env_file"

	TagReasonTemplated      = "templated_image"
	TagReasonFromDefault    = "tag_from_default"
	TagReasonSharedAnchor   = "shared_anchor"
	TagReasonSharedVariable = "shared_variable"
	TagReasonPinnedDigest   = "pinned_by_digest"
	TagReasonNotFound       = "service_not_in_compose_files"
	TagReasonUnreadable     = "compose_files_unreadable"
)

// SourceFile is a Compose or environment file's path and content.
type SourceFile struct {
	Path string
	Data []byte
}

// envTemplateRE matches exactly repo:${VAR} or repo:${VAR:-default}, where
// the variable is the whole tag and nothing else is templated.
var envTemplateRE = regexp.MustCompile(`^([^$]+):\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}$]*))?\}$`)

// imageSource is where a service's effective image: value comes from.
type imageSource struct {
	file     int        // index of the last file that sets it
	scalar   *yaml.Node // the scalar node (after alias resolution)
	anchored bool       // reached through an alias or merge key
}

// ClassifyTagSources classifies each service's tag source.
// files are the Compose files in -f order; envFiles the environment files
// in load order (later definitions win). The result is keyed by service.
func ClassifyTagSources(files []SourceFile, envFiles []SourceFile) (map[string]protocol.TagChange, error) {
	out, _, err := LocateTagSources(files, envFiles)
	return out, err
}

// TagLocation is where a service's tag is defined, for in-place edits.
type TagLocation struct {
	File    string // the Compose file whose image: scalar wins (source image)
	Line    int    // 1-based position of that scalar
	Column  int
	Value   string // the scalar as parsed
	VarFile string // the env file that defines Variable last (source env_file)
	// Tag is the tag the files name for the service, "" when it cannot be told.
	Tag string
}

// LocateTagSources classifies each service's tag source and also returns
// where it is, for in-place edits.
func LocateTagSources(files []SourceFile, envFiles []SourceFile) (map[string]protocol.TagChange, map[string]TagLocation, error) {
	sources := map[string]imageSource{}
	serviceNodes := map[string][]*yaml.Node{} // every definition of each service, for variable-use scans
	scalarUsers := map[*yaml.Node][]string{}  // anchored scalar → services resolving to it

	for i, f := range files {
		var doc yaml.Node
		if err := yaml.Unmarshal(f.Data, &doc); err != nil {
			return nil, nil, fmt.Errorf("parse %s: %w", f.Path, err)
		}
		services := mapValue(docRoot(&doc), "services")
		if services == nil || services.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j+1 < len(services.Content); j += 2 {
			name := services.Content[j].Value
			svc := resolve(services.Content[j+1])
			if svc == nil || svc.Kind != yaml.MappingNode {
				continue
			}
			serviceNodes[name] = append(serviceNodes[name], svc)
			if scalar, anchored := imageScalar(svc); scalar != nil {
				// A later file overrides an earlier one.
				sources[name] = imageSource{file: i, scalar: scalar, anchored: anchored}
			}
		}
	}
	for name, src := range sources {
		scalarUsers[src.scalar] = append(scalarUsers[src.scalar], name)
	}
	env, envSource := parseEnvFiles(envFiles)

	out := map[string]protocol.TagChange{}
	locs := map[string]TagLocation{}
	for name, src := range sources {
		value := src.scalar.Value
		tc := protocol.TagChange{}
		switch {
		case !strings.Contains(value, "$"):
			tc.Source = TagSourceImage
			tc.Supported = true
			if strings.Contains(value, "@") {
				tc.Supported, tc.Reason = false, TagReasonPinnedDigest
			}
		default:
			m := envTemplateRE.FindStringSubmatch(value)
			if m == nil {
				tc.Reason = TagReasonTemplated
				break
			}
			variable, hasDefault := m[2], m[3] != ""
			if _, defined := env[variable]; !defined {
				if hasDefault {
					tc.Reason = TagReasonFromDefault
				} else {
					tc.Reason = TagReasonTemplated // defined only in the shell or agent environment
				}
				break
			}
			tc.Source, tc.Variable, tc.Supported = TagSourceEnvFile, variable, true
			for other, nodes := range serviceNodes {
				if other != name && usesVariable(nodes, variable) {
					tc.Supported, tc.Reason = false, TagReasonSharedVariable
					break
				}
			}
		}
		if tc.Supported && src.anchored && len(scalarUsers[src.scalar]) > 1 {
			tc.Supported, tc.Reason = false, TagReasonSharedAnchor
		}
		out[name] = tc
		loc := TagLocation{File: files[src.file].Path, Line: src.scalar.Line, Column: src.scalar.Column, Value: src.scalar.Value}
		if tc.Variable != "" {
			loc.VarFile = envSource[tc.Variable]
			loc.Tag = envValue(env[tc.Variable])
		} else if !strings.Contains(value, "$") {
			if n, err := reference.ParseNormalizedNamed(value); err == nil {
				if t, ok := reference.TagNameOnly(n).(reference.Tagged); ok {
					loc.Tag = t.Tag()
				}
			}
		}
		locs[name] = loc
	}
	return out, locs, nil
}

// docRoot returns the top-level node of a parsed document.
func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return resolve(doc.Content[0])
	}
	return resolve(doc)
}

func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i].Tag != "!!merge" {
			return resolve(m.Content[i+1])
		}
	}
	return nil
}

// imageScalar finds a service's image: scalar, directly or through merge
// keys (<<: *anchor), and reports whether an anchor was involved.
func imageScalar(svc *yaml.Node) (*yaml.Node, bool) {
	for i := 0; i+1 < len(svc.Content); i += 2 {
		k, v := svc.Content[i], svc.Content[i+1]
		if k.Value == "image" && k.Tag != "!!merge" {
			anchored := v.Kind == yaml.AliasNode || v.Anchor != ""
			v = resolve(v)
			if v == nil || v.Kind != yaml.ScalarNode {
				return nil, false
			}
			return v, anchored
		}
	}
	// Merge keys: earlier entries in a merge sequence take precedence.
	for i := 0; i+1 < len(svc.Content); i += 2 {
		k, v := svc.Content[i], svc.Content[i+1]
		if k.Tag != "!!merge" && k.Value != "<<" {
			continue
		}
		var bases []*yaml.Node
		if v.Kind == yaml.SequenceNode {
			bases = v.Content
		} else {
			bases = []*yaml.Node{v}
		}
		for _, b := range bases {
			if base := resolve(b); base != nil && base.Kind == yaml.MappingNode {
				if s, _ := imageScalar(base); s != nil {
					return s, true
				}
			}
		}
	}
	return nil, false
}

// usesVariable reports whether any scalar in the nodes references variable.
func usesVariable(nodes []*yaml.Node, variable string) bool {
	re := regexp.MustCompile(`\$\{?` + regexp.QuoteMeta(variable) + `\b`)
	var walk func(n *yaml.Node) bool
	seen := map[*yaml.Node]bool{}
	walk = func(n *yaml.Node) bool {
		n = resolve(n)
		if n == nil || seen[n] {
			return false
		}
		seen[n] = true
		if n.Kind == yaml.ScalarNode {
			return re.MatchString(n.Value)
		}
		for _, c := range n.Content {
			if walk(c) {
				return true
			}
		}
		return false
	}
	for _, n := range nodes {
		if walk(n) {
			return true
		}
	}
	return false
}

// parseEnvFiles returns the variables defined in Compose environment files
// (KEY=value lines; blank lines and # comments ignored; optional "export ").
func parseEnvFiles(files []SourceFile) (map[string]string, map[string]string) {
	vars, source := map[string]string{}, map[string]string{}
	for _, f := range files {
		sc := bufio.NewScanner(bytes.NewReader(f.Data))
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			vars[strings.TrimSpace(k)] = strings.TrimSpace(v)
			source[strings.TrimSpace(k)] = f.Path
		}
	}
	return vars, source
}

// envValue is the value Compose reads from the text after KEY=: a quoted value
// without its quotes, or an unquoted one without a trailing " # comment".
func envValue(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') {
		if i := strings.IndexByte(s[1:], s[0]); i >= 0 {
			return s[1 : i+1]
		}
	}
	if i := strings.Index(s, " #"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// EnvFilePaths returns the environment files Compose used: the labelled
// ones, or <working_dir>/.env, which Compose reads by default and does not
// label (checked on Compose v5.2.0).
func EnvFilePaths(ci *protocol.ComposeInfo) []string {
	if ps := splitList(ci.EnvironmentFile); len(ps) > 0 {
		return ps
	}
	return []string{path.Join(ci.WorkingDir, ".env")}
}
