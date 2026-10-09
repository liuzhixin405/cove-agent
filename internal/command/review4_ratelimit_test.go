package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/liuzhixin405/cove-agent/internal/api"
)

// The reset durations in the snapshot are relative to the response that
// carried them; /ratelimit used to print them unchanged minutes later.
func TestRateLimitCmdCountsDownFromTheSnapshotTime(t *testing.T) {
	cmd := NewRateLimitCmd()
	eng := &fakeEngine{rlInfo: api.RateLimitInfo{RequestsLimit: 50, RequestsRemaining: 0, RequestsReset: time.Minute, UpdatedAt: time.Now().Add(-10 * time.Second)}}
	out, err := cmd.Execute(context.Background(), Input{Engine: eng})
	if err != nil || strings.Contains(out.Message, "1m0s") || !strings.Contains(out.Message, "reset in") {
		t.Fatalf("ratelimit = %q, %v (want a countdown below 1m0s)", out.Message, err)
	}
	eng.rlInfo.UpdatedAt = time.Now().Add(-5 * time.Minute)
	out, _ = cmd.Execute(context.Background(), Input{Engine: eng})
	if !strings.Contains(out.Message, "已重置") || strings.Contains(out.Message, "reset in") {
		t.Fatalf("ratelimit after the window = %q, want 已重置", out.Message)
	}
}
