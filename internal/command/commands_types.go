package command

import (
	"sync"

	"github.com/liuzhixin405/cove-agent/internal/checkpoint"
)

type CommitCmd struct{}
type ReviewCmd struct{}
type DoctorCmd struct{}
type ConfigCmd struct{}
type CompactCmd struct{}
type CostCmd struct{}
type DiffCmd struct{}
type UndoCmd struct {
	mu           sync.Mutex
	preview      *checkpoint.FileRestorePlan
	previewScope string
}
type CheckpointsCmd struct{}
type RateLimitCmd struct{}
type MemoryCmd struct{}
type ResumeCmd struct{}
type HistoryCmd struct{}
type McpCmd struct{}
type PluginCmd struct{}
type SkillsCmd struct{}
type ExportCmd struct{}
type SystemCmd struct{}
type DreamCmd struct{}

type CdCmd struct{}
type ContextCmd struct{}

type PermissionsCmd struct{}
type StatusCmd struct{}
type StatsCmd struct{}
type InitCmd struct{}

func NewCommitCmd() Command      { return &CommitCmd{} }
func NewReviewCmd() Command      { return &ReviewCmd{} }
func NewDoctorCmd() Command      { return &DoctorCmd{} }
func NewConfigCmd() Command      { return &ConfigCmd{} }
func NewCompactCmd() Command     { return &CompactCmd{} }
func NewCostCmd() Command        { return &CostCmd{} }
func NewDiffCmd() Command        { return &DiffCmd{} }
func NewUndoCmd() Command        { return &UndoCmd{} }
func NewCheckpointsCmd() Command { return &CheckpointsCmd{} }
func NewRateLimitCmd() Command   { return &RateLimitCmd{} }
func NewMemoryCmd() Command      { return &MemoryCmd{} }
func NewResumeCmd() Command      { return &ResumeCmd{} }
func NewHistoryCmd() Command     { return &HistoryCmd{} }
func NewMcpCmd() Command         { return &McpCmd{} }
func NewPluginCmd() Command      { return &PluginCmd{} }
func NewSkillsCmd() Command      { return &SkillsCmd{} }
func NewExportCmd() Command      { return &ExportCmd{} }
func NewSystemCmd() Command      { return &SystemCmd{} }
func NewCdCmd() Command          { return &CdCmd{} }
func NewDreamCmd() Command       { return &DreamCmd{} }
func NewContextCmd() Command     { return &ContextCmd{} }
func NewPermissionsCmd() Command { return &PermissionsCmd{} }
func NewStatusCmd() Command      { return &StatusCmd{} }
func NewStatsCmd() Command       { return &StatsCmd{} }
func NewInitCmd() Command        { return &InitCmd{} }
