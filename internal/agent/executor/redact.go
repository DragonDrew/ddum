package executor

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// A value shorter than this, or a value that reads like a version, is not
// masked: it would hide more than it protects.
const minMaskedLength = 8

var versionLikeRE = regexp.MustCompile(`^v?\d[\w.+-]*$`)

// sensitiveValues collects the values of every stack's env files and Portainer
// variables, which command output can quote: Compose repeats a value it cannot
// parse, for instance. Output goes to the bot's database and on to Discord, so
// these are masked first. A secret written straight into a Compose file
// cannot be told from the rest of that file, so it is not covered.
func (x *Executor) sensitiveValues(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	seen := map[string]bool{}
	add := func(v string) {
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if len(v) >= minMaskedLength && !versionLikeRE.MatchString(v) {
			seen[v] = true
		}
	}
	for _, st := range x.Inventory() {
		if st.Compose == nil {
			continue
		}
		if st.Provider == protocol.ProviderPortainer {
			if id, ok := portainerStackID(st); ok && x.Portainer != nil {
				if ps, err := x.Portainer.Stack(ctx, id); err == nil {
					for _, e := range ps.Env {
						add(e.Value)
					}
				}
			}
			continue
		}
		for _, p := range discovery.EnvFilePaths(st.Compose) {
			data, err := x.Files.ReadFile(p)
			if err != nil {
				continue
			}
			for _, line := range strings.Split(string(data), "\n") {
				line = strings.TrimSpace(line)
				if _, v, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(line, "#") {
					add(v)
				}
			}
		}
	}
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	// Longest first, so a value inside a longer one cannot leave part of it.
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	return values
}

func maskValues(s string, values []string) string {
	for _, v := range values {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}
