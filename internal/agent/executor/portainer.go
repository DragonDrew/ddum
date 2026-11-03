package executor

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/distribution/reference"

	"github.com/DragonDrew/ddum/internal/agent/discovery"
	"github.com/DragonDrew/ddum/internal/agent/portainer"
	"github.com/DragonDrew/ddum/internal/protocol"
)

// PortainerAPI is the part of the Portainer API the executor uses.
type PortainerAPI interface {
	Stack(ctx context.Context, id int) (portainer.Stack, error)
	StackFile(ctx context.Context, id int) (string, error)
	UpdateFileStack(ctx context.Context, id int, content string, env []portainer.Pair) error
	RedeployGitStack(ctx context.Context, s portainer.Stack) error
}

// portainerPlan is a Portainer stack's current and new definition.
type portainerPlan struct {
	stack      portainer.Stack
	content    string
	newContent string
	newEnv     []portainer.Pair
}

// portainerStackID reads portainer_stack_id from a stack's provider_ref.
func portainerStackID(st protocol.Stack) (int, bool) {
	var ref struct {
		ID int `json:"portainer_stack_id"`
	}
	if json.Unmarshal(st.ProviderRef, &ref) != nil || ref.ID == 0 {
		return 0, false
	}
	return ref.ID, true
}

// planPortainer fills a stack plan for a Portainer stack: the services'
// pulls and tags as for Compose, and the tag-source edits applied to the
// stack file or its Env list in memory. Git-based stacks take
// same-tag digest updates only.
func (x *Executor) planPortainer(ctx context.Context, sp *stackPlan, t protocol.DeployTarget) error {
	if x.Portainer == nil {
		return fail(protocol.FailUnsupported, "stack %s is managed by Portainer, and the Portainer provider is not enabled", sp.stack.Fingerprint)
	}
	id, ok := portainerStackID(sp.stack)
	if !ok {
		return fail(protocol.FailIneligible, "stack %s has no Portainer stack ID", sp.stack.Fingerprint)
	}
	ps, err := x.Portainer.Stack(ctx, id)
	if err != nil {
		return fail(protocol.FailIneligible, "read Portainer stack %d: %v", id, err)
	}
	content, err := x.Portainer.StackFile(ctx, id)
	if err != nil {
		return fail(protocol.FailIneligible, "read Portainer stack file %d: %v", id, err)
	}
	pp := &portainerPlan{stack: ps, content: content, newContent: content, newEnv: append([]portainer.Pair(nil), ps.Env...)}
	var envLines []string
	for _, e := range ps.Env {
		envLines = append(envLines, e.Name+"="+e.Value)
	}
	classes, locs, err := discovery.LocateTagSources(
		[]discovery.SourceFile{{Path: discovery.PortainerFilePath(id), Data: []byte(content)}},
		[]discovery.SourceFile{{Path: discovery.PortainerEnvPath, Data: []byte(strings.Join(envLines, "\n"))}})
	if err != nil {
		return fail(protocol.FailIneligible, "parse the Portainer stack file: %v", err)
	}
	for i := range sp.services {
		s := &sp.services[i]
		if s.currentTag == s.desiredTag {
			continue
		}
		if ps.GitBased() {
			return fail(protocol.FailIneligible, "the tag of %s cannot be changed here: %s", s.change.IdentityKey, discovery.TagReasonGitManaged)
		}
		tc, ok := classes[s.change.IdentityKey]
		if !ok || !tc.Supported {
			return fail(protocol.FailIneligible, "the tag of %s cannot be changed here: %s", s.change.IdentityKey, tc.Reason)
		}
		loc := locs[s.change.IdentityKey]
		if tc.Source == discovery.TagSourceEnvFile {
			for j := range pp.newEnv {
				if pp.newEnv[j].Name == tc.Variable {
					pp.newEnv[j].Value = s.desiredTag
				}
			}
			continue
		}
		newValue := withTag(loc.Value, s.desiredTag)
		if n, err := reference.ParseNormalizedNamed(newValue); err != nil || n.Name() != s.repo {
			return fail(protocol.FailUnsafe, "the image: value of %s names another repository", s.change.IdentityKey)
		}
		edited, err := ReplaceYAMLScalar([]byte(pp.newContent), loc.Line, loc.Column, loc.Value, newValue)
		if err != nil {
			return fail(protocol.FailIneligible, "edit the stack file: %v", err)
		}
		pp.newContent = string(edited)
	}
	sp.portainer = pp
	return nil
}

// applyPortainer redeploys a Portainer stack through its own update
// endpoints, so Portainer keeps the stack's name, ID, and ownership.
func (x *Executor) applyPortainer(ctx context.Context, pp *portainerPlan) error {
	var err error
	if pp.stack.GitBased() {
		err = x.Portainer.RedeployGitStack(ctx, pp.stack)
	} else {
		err = x.Portainer.UpdateFileStack(ctx, pp.stack.ID, pp.newContent, pp.newEnv)
	}
	if x.OnPortainerChange != nil {
		x.OnPortainerChange(pp.stack.ID)
	}
	if err != nil {
		return fail(protocol.FailDeploy, "Portainer redeploy of stack %d: %v", pp.stack.ID, err)
	}
	return nil
}

// planPortainerRestore works out what a rollback sends to Portainer: the stack
// as it is now, with only the tags put back, so changes to the stack
// file or its variables since the deploy stay. A Git-based stack is just
// redeployed: its deploys never change a tag.
func (x *Executor) planPortainerRestore(ctx context.Context, m *snapshotMeta, st protocol.Stack) (*portainerPlan, error) {
	if x.Portainer == nil {
		return nil, fail(protocol.FailUnsupported, "the Portainer provider is not enabled")
	}
	ps, err := x.Portainer.Stack(ctx, m.PortainerStackID)
	if err != nil {
		return nil, fail(protocol.FailIneligible, "read Portainer stack %d: %v", m.PortainerStackID, err)
	}
	pp := &portainerPlan{stack: ps}
	if ps.GitBased() {
		return pp, nil
	}
	content, err := x.Portainer.StackFile(ctx, ps.ID)
	if err != nil {
		return nil, fail(protocol.FailIneligible, "read Portainer stack file %d: %v", ps.ID, err)
	}
	var envLines []string
	for _, e := range ps.Env {
		envLines = append(envLines, e.Name+"="+e.Value)
	}
	edits, err := restoreEdits(m, st,
		[]discovery.SourceFile{{Path: discovery.PortainerFilePath(ps.ID), Data: []byte(content)}},
		[]discovery.SourceFile{{Path: discovery.PortainerEnvPath, Data: []byte(strings.Join(envLines, "\n"))}})
	if err != nil {
		return nil, err
	}
	pp.newContent, pp.newEnv = content, append([]portainer.Pair(nil), ps.Env...)
	for _, e := range edits {
		if e.variable != "" {
			for j := range pp.newEnv {
				if pp.newEnv[j].Name == e.variable {
					pp.newEnv[j].Value = e.newValue
				}
			}
			continue
		}
		edited, err := ReplaceYAMLScalar([]byte(pp.newContent), e.line, e.column, e.oldValue, e.newValue)
		if err != nil {
			return nil, fail(protocol.FailIneligible, "edit the stack file: %v", err)
		}
		pp.newContent = string(edited)
	}
	return pp, nil
}
