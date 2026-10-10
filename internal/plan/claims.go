package plan

import (
	"context"
	"sync"
)

// WriteClaims is the set of files the tasks of one parallel plan have
// written, by task. Sub-agents share one working tree, so without it two
// tasks could write the same file and the later one silently won.
type WriteClaims struct {
	mu    sync.Mutex
	owner map[string]string
}

func NewWriteClaims() *WriteClaims { return &WriteClaims{owner: map[string]string{}} }

// Claim records key for task. ok is false, with the owning task, when another
// task already wrote that key in this plan.
func (c *WriteClaims) Claim(key, task string) (ok bool, owner string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if prev, exists := c.owner[key]; exists && prev != task {
		return false, prev
	}
	c.owner[key] = task
	return true, task
}

type claimsKey struct{}

type taskClaims struct {
	task   string
	claims *WriteClaims
}

// WithTaskClaims marks ctx as running task inside a parallel plan whose
// write claims are claims.
func WithTaskClaims(ctx context.Context, task string, claims *WriteClaims) context.Context {
	return context.WithValue(ctx, claimsKey{}, taskClaims{task: task, claims: claims})
}

// ClaimPath claims key for the task ctx runs; true when ctx is not inside a
// parallel plan (nothing to coordinate) or the claim succeeds.
func ClaimPath(ctx context.Context, key string) (ok bool, owner string) {
	tc, in := ctx.Value(claimsKey{}).(taskClaims)
	if !in || tc.claims == nil {
		return true, ""
	}
	return tc.claims.Claim(key, tc.task)
}

// taskIDFrom is the task ctx runs inside a parallel plan, or "".
func taskIDFrom(ctx context.Context) string {
	tc, _ := ctx.Value(claimsKey{}).(taskClaims)
	return tc.task
}
