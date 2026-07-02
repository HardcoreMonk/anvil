package main

import (
	"context"
	"testing"
)

func TestClientNameContext(t *testing.T) {
	ctx := context.Background()
	if got := clientNameFromContext(ctx); got != "" {
		t.Errorf("bare ctx: got %q, want empty", got)
	}
	ctx = withClientName(ctx, "alice")
	if got := clientNameFromContext(ctx); got != "alice" {
		t.Errorf("got %q, want alice", got)
	}
}

func TestClientHolderContext(t *testing.T) {
	ctx := context.Background()
	if h := clientHolderFromContext(ctx); h != nil {
		t.Error("bare ctx should carry no holder")
	}
	h := &clientHolder{}
	ctx = withClientHolder(ctx, h)
	got := clientHolderFromContext(ctx)
	if got != h {
		t.Fatal("holder not round-tripped by pointer")
	}
	got.name = "bob"
	if h.name != "bob" {
		t.Error("holder must be shared by pointer (auth writes, audit reads)")
	}
}
