package hub

import (
	"testing"

	"github.com/gravel-project/gravel/drivers"
)

// TestFeedDelivery: the delivery to the hub is the endpoint whose host is the hub's feed URL's,
// or the only one; with several and none the hub's, none.
func TestFeedDelivery(t *testing.T) {
	hubs := drivers.FeedDelivery{Host: "ingest.example.org", Sent: 4}
	other := drivers.FeedDelivery{Host: "stats.example.net", Sent: 9}
	for _, c := range []struct {
		feeds []drivers.FeedDelivery
		url   string
		want  drivers.FeedDelivery
		ok    bool
	}{
		{[]drivers.FeedDelivery{other, hubs}, "https://Ingest.Example.org/api/ingest/events", hubs, true},
		{[]drivers.FeedDelivery{hubs}, "https://elsewhere.example.com", hubs, true},
		{[]drivers.FeedDelivery{other, hubs}, "https://elsewhere.example.com", drivers.FeedDelivery{}, false},
		{nil, "https://ingest.example.org", drivers.FeedDelivery{}, false},
	} {
		if got, ok := feedDelivery(c.feeds, c.url); got != c.want || ok != c.ok {
			t.Errorf("feedDelivery(%v, %q) = %+v, %v", c.feeds, c.url, got, ok)
		}
	}
}
