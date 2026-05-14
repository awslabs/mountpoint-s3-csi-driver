package main

import (
	"testing"
)

func TestTailBuf_UnderCapacity(t *testing.T) {
	tb := newTailBuf(10)
	tb.Write([]byte("hello"))
	got := string(tb.Bytes())
	if got != "hello" {
		t.Fatalf("expected %q, got %q", "hello", got)
	}
}

func TestTailBuf_ExactCapacity(t *testing.T) {
	tb := newTailBuf(5)
	tb.Write([]byte("abcde"))
	got := string(tb.Bytes())
	if got != "abcde" {
		t.Fatalf("expected %q, got %q", "abcde", got)
	}
}

func TestTailBuf_OverflowSingleWrite(t *testing.T) {
	tb := newTailBuf(5)
	tb.Write([]byte("abcdefgh"))
	got := string(tb.Bytes())
	if got != "defgh" {
		t.Fatalf("expected %q, got %q", "defgh", got)
	}
}

func TestTailBuf_OverflowMultipleWrites(t *testing.T) {
	tb := newTailBuf(5)
	tb.Write([]byte("abc"))
	tb.Write([]byte("defgh"))
	got := string(tb.Bytes())
	if got != "defgh" {
		t.Fatalf("expected %q, got %q", "defgh", got)
	}
}

func TestTailBuf_WrapAround(t *testing.T) {
	tb := newTailBuf(5)
	tb.Write([]byte("abcd"))
	tb.Write([]byte("ef"))
	got := string(tb.Bytes())
	if got != "bcdef" {
		t.Fatalf("expected %q, got %q", "bcdef", got)
	}
}

func TestTailBuf_ManySmallWrites(t *testing.T) {
	tb := newTailBuf(4)
	for _, b := range []byte("abcdefghij") {
		tb.Write([]byte{b})
	}
	got := string(tb.Bytes())
	if got != "ghij" {
		t.Fatalf("expected %q, got %q", "ghij", got)
	}
}

func TestTailBuf_ZeroCapacity(t *testing.T) {
	tb := newTailBuf(0)
	n, err := tb.Write([]byte("data"))
	if err != nil || n != 4 {
		t.Fatalf("expected n=4 err=nil, got n=%d err=%v", n, err)
	}
	got := string(tb.Bytes())
	if got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
