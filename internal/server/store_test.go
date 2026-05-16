package server

import (
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFilter_Match(t *testing.T) {
	tests := []struct {
		name string
		f    Filter
		k    Knock
		want bool
	}{
		{"empty filter matches anything", Filter{}, Knock{Topic: "x", To: "alice"}, true},
		{"topic match", Filter{Topic: "ops"}, Knock{Topic: "ops"}, true},
		{"topic miss", Filter{Topic: "ops"}, Knock{Topic: "marketing"}, false},
		{"to direct match", Filter{To: "alice"}, Knock{To: "alice"}, true},
		{"to broadcast (k.To empty) reaches directed subscribers", Filter{To: "alice"}, Knock{To: ""}, true},
		{"to mismatch", Filter{To: "alice"}, Knock{To: "bob"}, false},
		{"topic+to both required, both match", Filter{Topic: "ops", To: "alice"}, Knock{Topic: "ops", To: "alice"}, true},
		{"topic+to both required, topic miss", Filter{Topic: "ops", To: "alice"}, Knock{Topic: "x", To: "alice"}, false},
		{"topic+to both required, to miss", Filter{Topic: "ops", To: "alice"}, Knock{Topic: "ops", To: "bob"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.f.Match(tt.k); got != tt.want {
				t.Errorf("Filter%+v.Match(%+v) = %v, want %v", tt.f, tt.k, got, tt.want)
			}
		})
	}
}

func TestNewID_Format(t *testing.T) {
	id := NewID()
	if len(id) != 28 {
		t.Errorf("len(NewID()) = %d, want 28", len(id))
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			t.Errorf("NewID has non-hex char %q in %q", c, id)
			break
		}
	}
}

func TestNewID_LexSortable(t *testing.T) {
	id1 := NewID()
	// 1 ms gap (not 1 µs) — under heavy CI load, two time.Now().UnixNano()
	// calls separated by a microsecond can land on the same nanosecond, in
	// which case the 6 random tiebreak bytes decide order ~50% of the time
	// and the test flakes.
	time.Sleep(time.Millisecond)
	id2 := NewID()
	if id1 >= id2 {
		t.Errorf("lex order broken: id1=%s, id2=%s", id1, id2)
	}
}

func TestNewID_Unique(t *testing.T) {
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		id := NewID()
		if _, dup := seen[id]; dup {
			t.Fatalf("collision at iter %d: %s", i, id)
		}
		seen[id] = struct{}{}
	}
}

func TestStore_RingEvictsOldest(t *testing.T) {
	s := NewMemStore(3)
	for i := 0; i < 5; i++ {
		s.Add(Knock{ID: pad(i), Topic: "t"})
	}
	got, _ := s.List(Filter{}, "", 100)
	if len(got) != 3 {
		t.Fatalf("after 5 adds with cap=3: len=%d, want 3", len(got))
	}
	wantIDs := []string{pad(2), pad(3), pad(4)}
	for i, k := range got {
		if k.ID != wantIDs[i] {
			t.Errorf("position %d: id=%s, want %s", i, k.ID, wantIDs[i])
		}
	}
}

func TestStore_List_SinceFilterLimit(t *testing.T) {
	s := NewMemStore(100)
	s.Add(Knock{ID: "id001", Topic: "ops", To: "alice"})
	s.Add(Knock{ID: "id002", Topic: "marketing", To: "alice"})
	s.Add(Knock{ID: "id003", Topic: "ops", To: ""})
	s.Add(Knock{ID: "id004", Topic: "ops", To: "bob"})

	t.Run("since is exclusive", func(t *testing.T) {
		got, _ := s.List(Filter{}, "id002", 100)
		if len(got) != 2 || got[0].ID != "id003" || got[1].ID != "id004" {
			t.Errorf("got %+v, want [id003, id004]", ids(got))
		}
	})

	t.Run("topic filter", func(t *testing.T) {
		got, _ := s.List(Filter{Topic: "ops"}, "", 100)
		if len(got) != 3 {
			t.Errorf("topic=ops: len=%d (%v), want 3", len(got), ids(got))
		}
	})

	t.Run("to filter includes broadcasts", func(t *testing.T) {
		got, _ := s.List(Filter{To: "alice"}, "", 100)
		// matches: id001 (alice), id002 (alice), id003 (broadcast); excludes id004 (bob)
		if len(got) != 3 {
			t.Errorf("to=alice: len=%d (%v), want 3", len(got), ids(got))
		}
	})

	t.Run("limit caps", func(t *testing.T) {
		got, _ := s.List(Filter{}, "", 2)
		if len(got) != 2 {
			t.Errorf("limit=2: len=%d, want 2", len(got))
		}
	})

	t.Run("limit≤0 defaults to 100", func(t *testing.T) {
		got, _ := s.List(Filter{}, "", 0)
		if len(got) != 4 {
			t.Errorf("limit=0: len=%d, want 4 (default)", len(got))
		}
		got, _ = s.List(Filter{}, "", -5)
		if len(got) != 4 {
			t.Errorf("limit=-5: len=%d, want 4 (default)", len(got))
		}
	})
}

func TestStore_SubscribeReceivesAdd(t *testing.T) {
	s := NewMemStore(100)
	ch, cancel := s.Subscribe()
	defer cancel()

	go s.Add(Knock{ID: "a"})

	select {
	case got := <-ch:
		if got.ID != "a" {
			t.Errorf("received id=%s, want a", got.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("no event in 1s")
	}
}

func TestStore_CancelIdempotentAndClosesChannel(t *testing.T) {
	s := NewMemStore(10)
	ch, cancel := s.Subscribe()
	cancel()
	cancel() // must not panic or double-close

	// channel is closed
	if _, ok := <-ch; ok {
		t.Error("channel should be closed after cancel")
	}
}

func TestStore_SlowConsumerDropsRatherThanBlock(t *testing.T) {
	s := NewMemStore(1000)
	ch, cancel := s.Subscribe()

	const fired = 200
	done := make(chan struct{})
	go func() {
		for i := 0; i < fired; i++ {
			s.Add(Knock{ID: pad(i), Topic: "t"})
		}
		close(done)
	}()

	select {
	case <-done:
		// good — Add never blocked.
	case <-time.After(2 * time.Second):
		t.Fatal("Add blocked on slow consumer (expected drops, got deadlock)")
	}

	// Close the channel via cancel, then drain — count must not exceed buffer.
	cancel()
	count := 0
	for range ch {
		count++
	}
	if count > 64 {
		t.Errorf("subscriber received %d items, want ≤64 (channel buffer)", count)
	}
	if count == 0 {
		t.Error("subscriber received 0 items, want >0 (some delivered before drops)")
	}
}

// TestStore_RaceAddSubscribe drives the subscriber lifecycle concurrently with
// producers. Run under -race to catch any send-on-closed-channel or torn map
// access between Add's send loop and Subscribe's cancel().
func TestStore_RaceAddSubscribe(t *testing.T) {
	s := NewMemStore(10000)
	const producers = 10
	const itemsPerProducer = 100
	const subCycles = 100

	var wg sync.WaitGroup
	wg.Add(producers + 1)

	for p := 0; p < producers; p++ {
		go func(p int) {
			defer wg.Done()
			for i := 0; i < itemsPerProducer; i++ {
				s.Add(Knock{ID: pad(p*itemsPerProducer + i), Topic: "t"})
			}
		}(p)
	}

	go func() {
		defer wg.Done()
		for i := 0; i < subCycles; i++ {
			ch, cancel := s.Subscribe()
			drained := make(chan struct{})
			go func() {
				for range ch {
					// drain until closed
				}
				close(drained)
			}()
			cancel()
			<-drained
		}
	}()

	wg.Wait()
}

func TestStore_Topics_DistinctFromRingBuffer(t *testing.T) {
	s := NewMemStore(100)
	s.Add(Knock{ID: pad(1), Topic: "ops"})
	s.Add(Knock{ID: pad(2), Topic: "marketing"})
	s.Add(Knock{ID: pad(3), Topic: "ops"})
	s.Add(Knock{ID: pad(4), Topic: "agents"})

	got, _ := s.Topics()
	want := map[string]bool{"main": true, "ops": true, "marketing": true, "agents": true}
	if len(got) != len(want) {
		t.Fatalf("Topics()=%v, want keys %v", got, want)
	}
	for _, topic := range got {
		if !want[topic] {
			t.Errorf("Topics() returned unexpected %q", topic)
		}
		delete(want, topic)
	}
	for missing := range want {
		t.Errorf("Topics() missing %q", missing)
	}
}

func TestStore_Topics_AlwaysIncludesMain(t *testing.T) {
	s := NewMemStore(100)
	// No knocks added at all.
	got, _ := s.Topics()
	if len(got) != 1 || got[0] != "main" {
		t.Errorf("empty store Topics()=%v, want [main]", got)
	}

	// Even with non-main topics, main still appears.
	s.Add(Knock{ID: pad(1), Topic: "ops"})
	got, _ = s.Topics()
	found := false
	for _, topic := range got {
		if topic == "main" {
			found = true
		}
	}
	if !found {
		t.Errorf("Topics()=%v, missing always-present main", got)
	}
}

func TestStore_Topics_Sorted(t *testing.T) {
	s := NewMemStore(100)
	s.Add(Knock{ID: pad(1), Topic: "zebra"})
	s.Add(Knock{ID: pad(2), Topic: "alpha"})
	s.Add(Knock{ID: pad(3), Topic: "main"}) // also added by user; should dedupe
	s.Add(Knock{ID: pad(4), Topic: "beta"})

	got, _ := s.Topics()
	want := []string{"alpha", "beta", "main", "zebra"}
	if len(got) != len(want) {
		t.Fatalf("Topics()=%v, want %v", got, want)
	}
	for i, topic := range got {
		if topic != want[i] {
			t.Errorf("Topics()[%d]=%q, want %q", i, topic, want[i])
		}
	}
}

func TestStore_Topics_IgnoresEmptyTopic(t *testing.T) {
	s := NewMemStore(100)
	// A direct store.Add with empty topic shouldn't surface as a "" channel
	// in the sidebar — the server defaults Topic at POST time, but the store
	// itself doesn't enforce that, so Topics() must filter.
	s.Add(Knock{ID: pad(1), Topic: ""})
	s.Add(Knock{ID: pad(2), Topic: "ops"})

	got, _ := s.Topics()
	for _, topic := range got {
		if topic == "" {
			t.Errorf("Topics()=%v, empty string leaked through", got)
		}
	}
}

// pad returns a fixed-width zero-padded decimal string. Used for synthetic IDs
// whose lexicographic order matches insertion order.
func pad(i int) string {
	const width = 28
	s := strconv.Itoa(i)
	if len(s) >= width {
		return s
	}
	return strings.Repeat("0", width-len(s)) + s
}

func ids(ks []Knock) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.ID
	}
	return out
}
