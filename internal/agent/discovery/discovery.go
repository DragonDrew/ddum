// Package discovery turns the containers the agent sees into the normalized
// Host → Stack → Container inventory. It is pure apart from the
// FileChecker it is given, so every rule is unit-tested.
package discovery

import (
	"encoding/json"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/DragonDrew/ddum/internal/agent/docker"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// Compose labels.
const (
	LabelComposeProject         = "com.docker.compose.project"
	LabelComposeService         = "com.docker.compose.service"
	LabelComposeContainerNumber = "com.docker.compose.container-number"
	LabelComposeOneoff          = "com.docker.compose.oneoff"
	LabelComposeConfigFiles     = "com.docker.compose.project.config_files"
	LabelComposeWorkingDir      = "com.docker.compose.project.working_dir"
	LabelComposeEnvironmentFile = "com.docker.compose.project.environment_file"
)

// ddum labels.
const (
	LabelPrefix    = "ddum."
	LabelStack     = "ddum.stack"
	LabelSelf      = "ddum.self"
	LabelProtected = "ddum.protected"
)

// portainerComposeDir is where Portainer keeps the Compose files of the
// stacks it deploys.
const portainerComposeDir = "/data/compose/"

// FileChecker resolves and checks paths named by Compose labels. The real
// implementation uses the agent's filesystem; tests use a map.
type FileChecker interface {
	// Resolve returns the path with symlinks resolved, or an error if it
	// does not exist or cannot be read.
	Resolve(p string) (string, error)
	// ReadFile returns the content of a path that Resolve accepted.
	ReadFile(p string) ([]byte, error)
}

// Options are the agent settings that affect discovery.
type Options struct {
	EnabledProviders    []string // portainer, compose, docker
	ComposeAllowedRoots []string
	Files               FileChecker
	// Portainer holds the configured environment's stacks by lower-case name
	// when the Portainer provider is enabled and reachable.
	Portainer map[string]PortainerStack
}

// PortainerStack is what discovery needs to know about one Portainer stack.
type PortainerStack struct {
	ID         int
	EndpointID int
	Git        bool
	Content    string   // the stack file (file-based stacks)
	Env        []string // "NAME=value" entries of the stack's Env list
}

// OwnLabels returns a container's labels without the ddum and Compose labels
// it only has because its image sets them. Docker merges an image's labels into
// its containers', so without this an image could hide its own updates with
// ddum.ignore, pose as the ddum bot, or join another Compose project.
// Labels the owner set are kept: a Compose project's own labels always have
// values no image would have built in.
func OwnLabels(labels, image map[string]string) map[string]string {
	var out map[string]string
	for k, v := range image {
		if got, ok := labels[k]; !ok || got != v || !(strings.HasPrefix(k, LabelPrefix) || strings.HasPrefix(k, "com.docker.compose.")) {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(labels))
			for lk, lv := range labels {
				out[lk] = lv
			}
		}
		delete(out, k)
	}
	if out == nil {
		return labels
	}
	return out
}

// Observed is one container plus its image, as read from Docker.
type Observed struct {
	Container docker.Container
	Image     *docker.Image // nil if the image is gone
}

// Discover groups observed containers into stacks. The result is sorted by
// fingerprint, and each stack's containers by identity key.
func Discover(observed []Observed, opt Options) []protocol.Stack {
	type group struct {
		stack   protocol.Stack
		members map[string][]Observed // identity key → containers
	}
	groups := map[string]*group{}
	get := func(fp, name, provider string) *group {
		g, ok := groups[fp]
		if !ok {
			g = &group{
				stack:   protocol.Stack{Fingerprint: fp, DisplayName: name, Provider: provider},
				members: map[string][]Observed{},
			}
			groups[fp] = g
		}
		return g
	}

	for _, o := range observed {
		c := o.Container
		l := c.Labels
		if strings.EqualFold(l[LabelComposeOneoff], "true") {
			continue // `docker compose run` containers are not part of the stack
		}
		switch {
		case l[LabelComposeProject] != "":
			project := l[LabelComposeProject]
			g := get("compose:"+project, project, protocol.ProviderCompose)
			if g.stack.Compose == nil {
				g.stack.Compose = composeInfo(l)
			}
			key := l[LabelComposeService]
			if key == "" {
				key = c.Name
			}
			g.members[key] = append(g.members[key], o)
		case l[LabelStack] != "":
			name := l[LabelStack]
			g := get("label:"+name, name, protocol.ProviderDocker)
			g.members[c.Name] = append(g.members[c.Name], o)
		default:
			g := get("container:"+c.Name, c.Name, protocol.ProviderDocker)
			g.members[c.Name] = append(g.members[c.Name], o)
		}
	}

	out := make([]protocol.Stack, 0, len(groups))
	for _, g := range groups {
		s := g.stack
		for key, members := range g.members {
			c := container(key, members)
			s.Containers = append(s.Containers, c)
		}
		sort.Slice(s.Containers, func(i, j int) bool { return s.Containers[i].IdentityKey < s.Containers[j].IdentityKey })
		classify(&s, opt)
		classifyTags(&s, opt)
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Fingerprint < out[j].Fingerprint })
	return out
}

// container builds the reported container for one identity key. A Compose
// service with several containers is reported once, from its lowest-numbered
// container, with the replicas_unsupported note.
func container(key string, members []Observed) protocol.Container {
	sort.SliceStable(members, func(i, j int) bool {
		return containerNumber(members[i].Container) < containerNumber(members[j].Container)
	})
	o := members[0]
	c := o.Container
	pc := protocol.Container{
		IdentityKey:  key,
		DockerID:     c.ID,
		DockerName:   c.Name,
		ImageRef:     c.ImageRef,
		ImageID:      c.ImageID,
		RepoDigests:  []string{},
		State:        c.State,
		Health:       c.Health,
		RestartCount: c.RestartCount,
		DdumLabels:   map[string]string{},
		Notes:        []string{},
	}
	if !c.CreatedAt.IsZero() {
		pc.CreatedAt = c.CreatedAt.UTC().Format(time.RFC3339)
	}
	if o.Image != nil {
		if o.Image.RepoDigests != nil {
			pc.RepoDigests = append([]string(nil), o.Image.RepoDigests...)
			sort.Strings(pc.RepoDigests)
		}
		pc.Platform = platform(*o.Image)
	}
	// Only ddum.* labels are reported.
	for k, v := range c.Labels {
		if strings.HasPrefix(k, LabelPrefix) {
			pc.DdumLabels[k] = v
		}
	}
	switch c.Labels[LabelSelf] {
	case protocol.SelfAgent:
		pc.Self = protocol.SelfAgent
		pc.Protected = true
	case protocol.SelfBot:
		pc.Self = protocol.SelfBot
	}
	if strings.EqualFold(c.Labels[LabelProtected], "true") {
		pc.Protected = true
	}
	if len(members) > 1 {
		pc.Notes = append(pc.Notes, protocol.NoteReplicasUnsupported)
	}
	return pc
}

func containerNumber(c docker.Container) int {
	n, err := strconv.Atoi(c.Labels[LabelComposeContainerNumber])
	if err != nil {
		return 1 << 30 // unnumbered containers sort last
	}
	return n
}

func platform(img docker.Image) string {
	if img.OS == "" || img.Arch == "" {
		return ""
	}
	p := img.OS + "/" + img.Arch
	if img.Variant != "" {
		p += "/" + img.Variant
	}
	return p
}

func composeInfo(l map[string]string) *protocol.ComposeInfo {
	return &protocol.ComposeInfo{
		Project:         l[LabelComposeProject],
		ConfigFiles:     splitList(l[LabelComposeConfigFiles]),
		WorkingDir:      l[LabelComposeWorkingDir],
		EnvironmentFile: l[LabelComposeEnvironmentFile],
	}
}

// classify sets the stack's provider and deploy support.
func classify(s *protocol.Stack, opt Options) {
	s.Deploy = protocol.Support{Supported: false}
	if s.Provider == protocol.ProviderDocker {
		s.Deploy.Reason = protocol.ReasonStandaloneManual
		return
	}

	ci := s.Compose
	if ps, ok := opt.Portainer[strings.ToLower(ci.Project)]; ok {
		// Matched to a Portainer stack of the configured environment: it is
		// deployed through the Portainer API, not from files.
		s.Provider = protocol.ProviderPortainer
		kind := "file"
		if ps.Git {
			kind = "git"
		}
		s.ProviderRef, _ = json.Marshal(map[string]any{"portainer_stack_id": ps.ID, "portainer_endpoint_id": ps.EndpointID,
			"portainer_stack_type": kind})
		for _, c := range s.Containers {
			if hasNote(c, protocol.NoteReplicasUnsupported) {
				s.Deploy.Reason = protocol.ReasonReplicasUnsupported + ": service " + c.IdentityKey
				return
			}
		}
		s.Deploy.Supported = true
		return
	}
	if isPortainerManaged(ci) {
		// Without the Portainer API, a Portainer stack is recognized by where
		// its Compose files live, and is not deployable.
		s.Provider = protocol.ProviderPortainer
		s.Deploy.Reason = protocol.ReasonPortainerNotEnabled
		if opt.Portainer != nil {
			s.Deploy.Reason = "not a stack of the configured Portainer environment"
		}
		return
	}
	if !has(opt.EnabledProviders, protocol.ProviderCompose) {
		s.Deploy.Reason = "compose provider not enabled"
		return
	}
	if reason := checkFiles(ci, opt); reason != "" {
		s.Deploy.Reason = reason
		return
	}
	ci.FilesAccessible = true
	for _, c := range s.Containers {
		if hasNote(c, protocol.NoteReplicasUnsupported) {
			s.Deploy.Reason = protocol.ReasonReplicasUnsupported + ": service " + c.IdentityKey
			return
		}
	}
	s.Deploy.Supported = true
}

func isPortainerManaged(ci *protocol.ComposeInfo) bool {
	for _, f := range ci.ConfigFiles {
		if strings.HasPrefix(f, portainerComposeDir) {
			return true
		}
	}
	return strings.HasPrefix(ci.WorkingDir+"/", portainerComposeDir)
}

// checkFiles returns "" if every Compose path is readable and, after symlink
// resolution, under an allowed root. Otherwise it
// returns a reason that names the path and the mount to add.
func checkFiles(ci *protocol.ComposeInfo, opt Options) string {
	if len(ci.ConfigFiles) == 0 || ci.WorkingDir == "" {
		return protocol.ReasonComposeFilesUnavailable + ": the containers carry no config_files or working_dir label"
	}
	if len(opt.ComposeAllowedRoots) == 0 {
		return protocol.ReasonComposeFilesUnavailable + ": COMPOSE_ALLOWED_ROOTS is not set"
	}
	paths := append([]string{ci.WorkingDir}, ci.ConfigFiles...)
	paths = append(paths, splitList(ci.EnvironmentFile)...)
	for _, raw := range paths {
		// Host paths are Linux paths; ToSlash only matters for tests on Windows.
		p := filepath.ToSlash(raw)
		if !path.IsAbs(p) && !filepath.IsAbs(raw) {
			return protocol.ReasonComposeFilesUnavailable + ": " + p + " is not an absolute path"
		}
		if !underRoots(path.Clean(p), opt.ComposeAllowedRoots) {
			return protocol.ReasonComposeFilesUnavailable + ": " + p + " is not under COMPOSE_ALLOWED_ROOTS"
		}
		if opt.Files == nil {
			return protocol.ReasonComposeFilesUnavailable + ": no filesystem access"
		}
		resolved, err := opt.Files.Resolve(raw)
		if err != nil {
			return protocol.ReasonComposeFilesUnavailable + ": " + p + " is not readable inside the agent; mount it at the same path"
		}
		if !underRoots(resolved, opt.ComposeAllowedRoots) {
			return protocol.ReasonComposeFilesUnavailable + ": " + p + " resolves to " + resolved + ", outside COMPOSE_ALLOWED_ROOTS"
		}
	}
	return ""
}

func underRoots(p string, roots []string) bool {
	for _, r := range roots {
		if p == r || strings.HasPrefix(p, strings.TrimSuffix(r, "/")+"/") {
			return true
		}
	}
	return false
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func has(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func hasNote(c protocol.Container, note string) bool {
	return has(c.Notes, note)
}

// classifyTags sets every container's tag_change. Only
// Compose stacks whose files the agent can read are classified; the rest
// report the stack's deploy reason.
func classifyTags(s *protocol.Stack, opt Options) {
	set := func(tc protocol.TagChange) {
		for i := range s.Containers {
			c := tc
			s.Containers[i].TagChange = &c
		}
	}
	if s.Provider == protocol.ProviderPortainer && s.Deploy.Supported {
		classifyPortainerTags(s, opt.Portainer[strings.ToLower(s.Compose.Project)], set)
		return
	}
	if s.Compose == nil || !s.Compose.FilesAccessible {
		set(protocol.TagChange{Reason: s.Deploy.Reason})
		return
	}
	var files []SourceFile
	for _, p := range s.Compose.ConfigFiles {
		data, err := opt.Files.ReadFile(p)
		if err != nil {
			set(protocol.TagChange{Reason: TagReasonUnreadable + ": " + p})
			return
		}
		files = append(files, SourceFile{Path: p, Data: data})
	}
	var envFiles []SourceFile
	for _, p := range EnvFilePaths(s.Compose) {
		if _, err := opt.Files.Resolve(p); err != nil {
			continue // the default .env is optional
		}
		if data, err := opt.Files.ReadFile(p); err == nil {
			envFiles = append(envFiles, SourceFile{Path: p, Data: data})
		}
	}
	byService, err := ClassifyTagSources(files, envFiles)
	if err != nil {
		set(protocol.TagChange{Reason: TagReasonUnreadable + ": " + err.Error()})
		return
	}
	for i := range s.Containers {
		tc, ok := byService[s.Containers[i].IdentityKey]
		if !ok {
			tc = protocol.TagChange{Reason: TagReasonNotFound}
		}
		s.Containers[i].TagChange = &tc
	}
}

// Portainer tag sources: the image: scalar in the stack file,
// or an entry of the stack's Env list.
const (
	TagSourcePortainerEnv = "portainer_env"
	TagReasonGitManaged   = "git_managed"
)

// classifyPortainerTags classifies a Portainer stack's services. Git-based
// stacks take same-tag digest updates only.
func classifyPortainerTags(s *protocol.Stack, ps PortainerStack, set func(protocol.TagChange)) {
	if ps.Git {
		set(protocol.TagChange{Reason: TagReasonGitManaged})
		return
	}
	env := SourceFile{Path: PortainerEnvPath, Data: []byte(strings.Join(ps.Env, "\n"))}
	byService, err := ClassifyTagSources([]SourceFile{{Path: PortainerFilePath(ps.ID), Data: []byte(ps.Content)}}, []SourceFile{env})
	if err != nil {
		set(protocol.TagChange{Reason: TagReasonUnreadable + ": " + err.Error()})
		return
	}
	for i := range s.Containers {
		tc, ok := byService[s.Containers[i].IdentityKey]
		if !ok {
			tc = protocol.TagChange{Reason: TagReasonNotFound}
		}
		if tc.Source == TagSourceEnvFile {
			tc.Source = TagSourcePortainerEnv
		}
		s.Containers[i].TagChange = &tc
	}
}

// PortainerEnvPath and PortainerFilePath name a Portainer stack's sources in
// tag-source classification; they are not filesystem paths.
const PortainerEnvPath = "portainer:env"

// PortainerFilePath names a Portainer stack file.
func PortainerFilePath(id int) string { return "portainer:stack/" + strconv.Itoa(id) }
