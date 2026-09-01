package main

import "testing"

func TestServiceInviteFromArgs(t *testing.T) {
	invite := "whiskerlink://connect?token=example&port=80&type=http"
	if got := serviceInviteFromArgs([]string{"whiskerlink", "--verbose", invite}); got != invite {
		t.Fatalf("serviceInviteFromArgs() = %q, want %q", got, invite)
	}
	if got := serviceInviteFromArgs([]string{"whiskerlink", "https://example.com"}); got != "" {
		t.Fatalf("serviceInviteFromArgs() accepted unrelated URL %q", got)
	}
}
