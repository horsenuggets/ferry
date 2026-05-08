package client

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/cenkalti/backoff/v4"
)

func TestIsRetryable_PolicyMatrix(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context-canceled", context.Canceled, false},
		{"deadline-exceeded", context.DeadlineExceeded, false},
		{"io-eof", io.EOF, true},
		{"unexpected-eof", io.ErrUnexpectedEOF, true},
		{"econnreset", syscall.ECONNRESET, true},
		{"econnrefused", syscall.ECONNREFUSED, true},
		{"etimedout", syscall.ETIMEDOUT, true},
		{"dns-error", &net.DNSError{Err: "no such host", Name: "x"}, true},
		{"random-error", errors.New("nope"), false},
		{"connection-reset-msg", errors.New("read tcp: connection reset by peer"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRetryable(c.err); got != c.want {
				t.Fatalf("IsRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestClassifyStatus(t *testing.T) {
	cases := []struct {
		status      int
		wantRetry   bool
		wantHeadRes bool
	}{
		{200, false, false}, // 2xx never reaches; documented as "not classify-able"
		{409, false, true},
		{500, true, false},
		{502, true, false},
		{503, true, false},
		{504, true, false},
		{401, false, false},
		{403, false, false},
		{400, false, false},
		{415, false, false},
		{408, true, false},
		{425, true, false},
		{429, true, false},
	}
	for _, c := range cases {
		retry, head := ClassifyStatus(c.status)
		if retry != c.wantRetry || head != c.wantHeadRes {
			t.Errorf("ClassifyStatus(%d) = (%v,%v), want (%v,%v)",
				c.status, retry, head, c.wantRetry, c.wantHeadRes)
		}
	}
}

func TestNewChunkBackoff_RespectsMaxRetries(t *testing.T) {
	bo := NewChunkBackoff()
	// Drive backoff manually: NextBackOff returns Stop after MaxChunkRetries.
	count := 0
	for {
		d := bo.NextBackOff()
		if d == backoff.Stop {
			break
		}
		// Defensive bound so a misconfigured policy can't loop forever.
		if count > int(MaxChunkRetries)+2 {
			t.Fatalf("backoff didn't stop after %d", count)
		}
		count++
		if d < 0 || d > 60*time.Second {
			t.Fatalf("unexpected backoff duration %v", d)
		}
	}
	if count != int(MaxChunkRetries) {
		t.Fatalf("expected %d retries, got %d", MaxChunkRetries, count)
	}
}
