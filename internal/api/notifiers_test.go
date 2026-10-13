package api_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	hubv1 "github.com/gravel-project/gravel/gen/gravel/hub/v1"
	"github.com/gravel-project/gravel/gen/gravel/hub/v1/hubv1connect"
	"github.com/gravel-project/gravel/internal/store"
)

// The notifiers round-trip for the owner, and nobody else reads where the hub's events go
// (gravel#196): not a visitor, not a member, not an app.
func TestNotifierSettings(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	sam := r.user(t, "Sam", "2")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	owner := hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)
	member := hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, sam.ID)), r.srv.URL)
	anon := hubv1connect.NewOrganizationServiceClient(http.DefaultClient, r.srv.URL)

	want := &hubv1.OrganizationSettings{Version: 1, Notifiers: []*hubv1.Notifier{
		{Name: "john-phone", Kind: "ntfy", Topic: "htg-ops", Events: &hubv1.NotifierEvents{Kinds: []string{"server", "backup"}, MinSeverity: "warning"}},
		{Name: "ops-hook", Kind: "webhook", Url: "https://ops.example.com/gravel", Events: &hubv1.NotifierEvents{Games: []string{"wardogs"}}},
		{Name: "discord", Kind: "webhook", Flavour: "discord", Disabled: true, Events: &hubv1.NotifierEvents{}},
		{Name: "mail", Kind: "email", To: []string{"ops@example.com"}, Events: &hubv1.NotifierEvents{Servers: []string{"htg-wardogs-1"}}},
	}}
	upd, err := owner.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: want}))
	if err != nil {
		t.Fatal(err)
	}
	if got := upd.Msg.GetSettings().GetNotifiers(); !slices.EqualFunc(got, want.GetNotifiers(), func(a, b *hubv1.Notifier) bool { return proto.Equal(a, b) }) {
		t.Errorf("updated:\n got %v\nwant %v", got, want.GetNotifiers())
	}
	got, err := owner.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
	if err != nil || len(got.Msg.GetSettings().GetNotifiers()) != 4 {
		t.Fatalf("the owner reads them: %v %v", got, err)
	}
	for name, c := range map[string]hubv1connect.OrganizationServiceClient{"a visitor": anon, "a member": member} {
		got, err := c.GetOrganizationSettings(ctx, connect.NewRequest(&hubv1.GetOrganizationSettingsRequest{}))
		if err != nil || got.Msg.GetSettings().GetVersion() != 1 || len(got.Msg.GetSettings().GetNotifiers()) != 0 {
			t.Errorf("%s reads the notifiers: %v %v", name, got, err)
		}
	}
	// A Discord webhook's URL is a secret: the settings refuse it.
	bad := proto.Clone(want).(*hubv1.OrganizationSettings)
	bad.Notifiers[2].Url = "https://discord.com/api/webhooks/1/token"
	if _, err := owner.UpdateOrganizationSettings(ctx, connect.NewRequest(&hubv1.UpdateOrganizationSettingsRequest{Settings: bad})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a Discord URL in the settings: %v", err)
	}
}

func TestTestNotifier(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	jo := r.user(t, "Jo", "1")
	sam := r.user(t, "Sam", "2")
	if _, err := r.org.Claim(ctx, r.token, store.Actor{UserID: &jo.ID}); err != nil {
		t.Fatal(err)
	}
	owner := hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, jo.ID)), r.srv.URL)
	member := hubv1connect.NewOrganizationServiceClient(cookieClient(http.DefaultClient, r.login(t, sam.ID)), r.srv.URL)
	req := func(name string) *connect.Request[hubv1.TestNotifierRequest] {
		return connect.NewRequest(&hubv1.TestNotifierRequest{Name: name})
	}

	if _, err := member.TestNotifier(ctx, req("john-phone")); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("a member: %v", err)
	}
	got, err := owner.TestNotifier(ctx, req(" john-phone "))
	if err != nil || !got.Msg.GetDelivered() || got.Msg.GetError() != "" {
		t.Fatalf("delivered: %v %v", got, err)
	}
	if _, err := owner.TestNotifier(ctx, req("nobody")); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("no such notifier: %v", err)
	}
	r.notifiers.err = errors.New("answered 401 Unauthorized")
	got, err = owner.TestNotifier(ctx, req("john-phone"))
	if err != nil || got.Msg.GetDelivered() || got.Msg.GetError() != "answered 401 Unauthorized" {
		t.Errorf("a failure is an answer, not an error: %v %v", got, err)
	}
	if r.notifiers.tested[0] != "john-phone@Test" {
		t.Errorf("tested %v", r.notifiers.tested)
	}
}
