package plan

import (
	"context"
	"testing"
)

func TestWriteClaimsOwnership(t *testing.T) {
	c := NewWriteClaims()
	if ok, _ := c.Claim("a.go", "t1"); !ok {
		t.Fatal("first claim must succeed")
	}
	if ok, _ := c.Claim("a.go", "t1"); !ok {
		t.Fatal("the same task may write its file again")
	}
	if ok, owner := c.Claim("a.go", "t2"); ok || owner != "t1" {
		t.Fatalf("second task must be refused with the owner, got ok=%v owner=%q", ok, owner)
	}
	if ok, _ := c.Claim("b.go", "t2"); !ok {
		t.Fatal("a different file is free")
	}
}

func TestClaimPathWithoutClaimsIsOpen(t *testing.T) {
	if ok, _ := ClaimPath(context.Background(), "x.go"); !ok {
		t.Fatal("no claims in ctx means no restriction")
	}
	claims := NewWriteClaims()
	ctx := WithTaskClaims(context.Background(), "t1", claims)
	if ok, _ := ClaimPath(ctx, "x.go"); !ok {
		t.Fatal("first claim through ctx")
	}
	ctx2 := WithTaskClaims(context.Background(), "t2", claims)
	if ok, owner := ClaimPath(ctx2, "x.go"); ok || owner != "t1" {
		t.Fatalf("conflict not detected: %v %q", ok, owner)
	}
}
