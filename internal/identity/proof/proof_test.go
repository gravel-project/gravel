package proof_test

import (
	"testing"

	"github.com/gravel-project/gravel/internal/identity/proof"
	"github.com/gravel-project/gravel/internal/identity/proof/prooftest"
)

func TestConformanceOnTheFakeStore(t *testing.T) { prooftest.Run(t, prooftest.FakeBackend) }

func TestGameChatCode(t *testing.T) {
	c := proof.GameChat{ProviderName: "steam"}
	for text, want := range map[string]string{
		"!link 7k3m-9qxa":     "7K3M-9QXA",
		".LINK 7K3M9QXA":      "7K3M-9QXA",
		"  !link  7K3M-9QXA ": "7K3M-9QXA",
		"!link 7K3M-9QX":      "",
		"!linked 7K3M-9QXA":   "",
		"say !link 7K3M-9QXA": "",
	} {
		got, ok := c.Code(text)
		if got != want || ok != (want != "") {
			t.Errorf("%q: %q %v, want %q", text, got, ok, want)
		}
	}
	if c.Name() != proof.GameChatName || c.Method() != proof.GameChatName || c.Provider() != "steam" {
		t.Errorf("channel %+v", c)
	}
}
