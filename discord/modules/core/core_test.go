package core_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/gravel-project/gravel/discord/modules/core"
	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
)

func TestMessages(t *testing.T) {
	at := timestamppb.New(time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC))
	u := &hubv1.User{DisplayName: "Jo", Owner: true, Identities: []*hubv1.Identity{
		{Provider: "discord", Subject: "42", DisplayName: "jo", LinkedAt: at},
		{Provider: "steam", Subject: "7656", LinkedAt: at},
		{Provider: "xbox", Subject: "x1", DisplayName: "JoX"},
	}}
	got := core.WhoamiMessage(u, "https://app.example.com/")
	for _, want := range []string{"**Jo** (owner)", "• Discord: jo (linked 2026-10-09)", "• Steam: 7656 (linked 2026-10-09)", "• xbox: JoX\n", "Manage them at https://app.example.com//account"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if !strings.Contains(core.LinkMessage("https://app.example.com"), "https://app.example.com/account") {
		t.Error("link message")
	}
	if !strings.Contains(core.NotLinkedMessage("https://app.example.com"), "/login") {
		t.Error("not-linked message")
	}
}
