package main

import "testing"

func TestInviteFromArgs(t *testing.T) {
	for _, invite := range []string{
		"whiskerlink://connect?token=example&port=80&type=http",
		"whiskerlink://receive?token=example",
	} {
		if got := inviteFromArgs([]string{"whiskerlink", "--verbose", invite}); got != invite {
			t.Fatalf("inviteFromArgs() = %q, want %q", got, invite)
		}
	}
	for _, unrelated := range []string{"https://example.com", "whiskerlink://unknown?token=example"} {
		if got := inviteFromArgs([]string{"whiskerlink", unrelated}); got != "" {
			t.Fatalf("inviteFromArgs() accepted unrelated URL %q", got)
		}
	}
}
