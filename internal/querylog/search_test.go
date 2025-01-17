package querylog

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQueryLog_Search_findClient(t *testing.T) {
	const knownClientID = "client-1"
	const knownClientName = "Known Client 1"
	const unknownClientID = "client-2"

	knownClient := &Client{
		IDs:  []string{knownClientID},
		Name: knownClientName,
	}

	findClientCalls := 0
	findClient := func(ids []string) (c *Client, _ error) {
		defer func() { findClientCalls++ }()

		if len(ids) == 0 {
			return nil, nil
		}

		if ids[0] == knownClientID {
			return knownClient, nil
		}

		return nil, nil
	}

	l := newQueryLog(Config{
		FindClient:        findClient,
		BaseDir:           t.TempDir(),
		RotationIvl:       1,
		MemSize:           100,
		Enabled:           true,
		FileEnabled:       true,
		AnonymizeClientIP: false,
	})
	t.Cleanup(l.Close)

	q := &dns.Msg{
		Question: []dns.Question{{
			Name: "example.com",
		}},
	}

	l.Add(AddParams{
		Question: q,
		ClientID: knownClientID,
		ClientIP: net.IP{1, 2, 3, 4},
	})

	// Add the same thing again to test the cache.
	l.Add(AddParams{
		Question: q,
		ClientID: knownClientID,
		ClientIP: net.IP{1, 2, 3, 4},
	})

	l.Add(AddParams{
		Question: q,
		ClientID: unknownClientID,
		ClientIP: net.IP{1, 2, 3, 5},
	})

	sp := &searchParams{
		// Add some time to the "current" one to protect against
		// low-resolution timers on some Windows machines.
		//
		// TODO(a.garipov): Use some kind of timeSource interface
		// instead of relying on time.Now() in tests.
		olderThan: time.Now().Add(10 * time.Second),
		limit:     3,
	}
	entries, _ := l.search(sp)
	assert.Equal(t, 2, findClientCalls)

	require.Len(t, entries, 3)

	assert.Nil(t, entries[0].client)

	gotClient := entries[2].client
	require.NotNil(t, gotClient)

	assert.Equal(t, knownClientName, gotClient.Name)
	assert.Equal(t, []string{knownClientID}, gotClient.IDs)
}
