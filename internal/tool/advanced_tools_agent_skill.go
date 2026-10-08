package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/liuzhixin405/cove-agent/internal/api"
	"github.com/liuzhixin405/cove-agent/internal/skills"
)

type SkillTool struct{ baseTool }
type AgentToolI struct{ baseTool }

func NewSkillTool() Tool {
	return &SkillTool{baseTool{def: Def{
		Name: "skill", Description: "Execute a skill (predefined workflow).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"args":{"type":"object"}},"required":["name"]}`),
		IsReadOnly:  false, PlanSafe: true, UserFacingName: "Skill",
	}}}
}
func (t *SkillTool) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	name, _ := input["name"].(string)
	if tctx.Runtime != nil {
		// Check SkillManager first: it holds the full skills.Skill struct
		// (Steps, AllowedTools), which RenderInvocation renders as an explicit
		// checklist/constraint. The flat SkillPrompts map (checked second)
		// only ever stores the bare prompt string, so it can't carry that
		// structure — it exists as a fallback for skills only registered
		// there. See skills.Skill.RenderInvocation and
		// docs/中等模型平替优化建议.md §Skills工作流化.
		if mgr, ok := tctx.Runtime.SkillManager.(interface {
			Get(string) (skills.Skill, bool)
		}); ok {
			if skill, found := mgr.Get(name); found {
				return Result{Data: skill.RenderInvocation()}, nil
			}
		}
		if tctx.Runtime.SkillPrompts != nil {
			if prompt, ok := tctx.Runtime.SkillPrompts[name]; ok {
				return Result{Data: fmt.Sprintf("[Skill: %s]\n\n%s\n\nFollow these instructions to complete the task.", name, prompt)}, nil
			}
		}
	}
	// Not found. This used to answer "Skill 'x' activated" (with every
	// skill's full prompt printed via %v) and no error flag, so the model
	// went on as if a nonexistent skill had run.
	msg := fmt.Sprintf("Error: skill %q not found.", name)
	if names := skillNames(tctx); len(names) > 0 {
		msg += " Available skills: " + strings.Join(names, ", ")
	} else {
		msg += " No skills are available."
	}
	return Result{Data: msg, IsError: true}, nil
}

// skillNames lists the skills the runtime knows, by name only.
func skillNames(tctx Context) []string {
	if tctx.Runtime == nil {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	if mgr, ok := tctx.Runtime.SkillManager.(interface{ All() []skills.Skill }); ok {
		for _, s := range mgr.All() {
			if !seen[s.Name] {
				seen[s.Name] = true
				names = append(names, s.Name)
			}
		}
	}
	for n := range tctx.Runtime.SkillPrompts {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}
func (t *SkillTool) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("skill execution is safe")
}

func NewAgentTool() Tool {
	return &AgentToolI{baseTool{def: Def{
		Name: "agent", Aliases: []string{"Agent"},
		Description: "Spawn a sub-agent to handle complex multi-step tasks independently.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string","enum":["general","explore","plan","review","test","verify"],"description":"Agent type: general (can edit), explore/plan/review (read-only), test, verify (independent Go red/green regression verification)"},"prompt":{"type":"string","description":"Task description for the sub-agent"}},"required":["type","prompt"]}`),
		IsReadOnly:  false, IsConcurrencySafe: true, PlanSafe: true, UserFacingName: "Agent",
	}}}
}
func (t *AgentToolI) Call(ctx context.Context, input Input, tctx Context) (Result, error) {
	agentType, _ := input["type"].(string)
	task, _ := input["prompt"].(string)
	if tctx.Runtime != nil && tctx.Runtime.AgentRunner != nil {
		if runner, ok := tctx.Runtime.AgentRunner.(api.AgentRunner); ok {
			result, err := runner.Run(ctx, agentType, task)
			if err != nil {
				return Result{Data: fmt.Sprintf("Sub-agent error: %v", err), IsError: true}, nil
			}
			return Result{Data: fmt.Sprintf("%s\nSub-agent [%s] result:\n%s\nCost: $%.4f | Steps: %d | Success: %v",
				agentExitLine(result), agentType, result.Output, result.Cost, result.Steps, result.Success), IsError: strings.EqualFold(agentType, "verify") && !result.Success}, nil
		}
	}
	return Result{Data: fmt.Sprintf("Sub-agent runner unavailable. Requested [%s]: %s", agentType, truncateStr(task, 300)), IsError: true}, nil
}

// agentExitLine is the first line of the agent tool's result, so the model
// sees at a glance whether the sub-agent finished or stopped early, e.g.
// "[exit: max_iterations, steps: 60, truncated: yes]". A runner that does not
// report an exit reason gets one derived from Success.
func agentExitLine(r *api.AgentRunResult) string {
	reason := r.ExitReason
	if reason == "" {
		reason = "error"
		if r.Success {
			reason = "completed"
		}
	}
	truncated := "no"
	if r.Truncated {
		truncated = "yes"
	}
	return fmt.Sprintf("[exit: %s, steps: %d, truncated: %s]", reason, r.Steps, truncated)
}

func (t *AgentToolI) CheckPermissions(input Input, tctx Context) PermissionDecision {
	return Allowed("agent spawning is safe")
}
