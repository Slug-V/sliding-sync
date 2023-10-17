package syncv3_test

import (
	"testing"
	"time"

	"github.com/matrix-org/complement/b"
	"github.com/matrix-org/complement/client"
	"github.com/matrix-org/sliding-sync/sync3"
	"github.com/matrix-org/sliding-sync/testutils/m"
)

// Make this graph:
//
//	   A       D      <-- parents
//	.--`--.    |
//	B     C    E   F  <-- children
//
// and query:
//
//	spaces[A] => B,C
//	spaces[D] => E
//	spaces[A,B] => B,C,E
func TestSpacesFilter(t *testing.T) {
	alice := registerNewUser(t)
	parentA := alice.MustCreateRoom(t, map[string]interface{}{
		"preset": "public_chat",
		"creation_content": map[string]string{
			"type": "m.space",
		},
	})
	parentD := alice.MustCreateRoom(t, map[string]interface{}{
		"preset": "public_chat",
		"creation_content": map[string]string{
			"type": "m.space",
		},
	})
	roomB := alice.MustCreateRoom(t, map[string]interface{}{
		"preset": "public_chat",
	})
	roomC := alice.MustCreateRoom(t, map[string]interface{}{
		"preset": "public_chat",
	})
	roomE := alice.MustCreateRoom(t, map[string]interface{}{
		"preset": "public_chat",
	})
	roomF := alice.MustCreateRoom(t, map[string]interface{}{
		"preset": "public_chat",
	})
	t.Logf("A: %s B: %s C: %s D: %s E: %s F: %s", parentA, roomB, roomC, parentD, roomE, roomF)
	alice.SendEventSynced(t, parentA, b.Event{
		Type:     "m.space.child",
		StateKey: &roomB,
		Content: map[string]interface{}{
			"via": []string{"example.com"},
		},
	})
	alice.SendEventSynced(t, parentA, b.Event{
		Type:     "m.space.child",
		StateKey: &roomC,
		Content: map[string]interface{}{
			"via": []string{"example.com"},
		},
	})
	alice.SendEventSynced(t, parentD, b.Event{
		Type:     "m.space.child",
		StateKey: &roomE,
		Content: map[string]interface{}{
			"via": []string{"example.com"},
		},
	})
	time.Sleep(100 * time.Millisecond) // let the proxy process this

	doSpacesListRequest := func(spaces []string, pos *string, listMatchers ...m.ListMatcher) *sync3.Response {
		t.Helper()
		var opts []client.RequestOpt
		if pos != nil {
			opts = append(opts, WithPos(*pos))
		}
		t.Logf("requesting rooms in spaces %v", spaces)
		res := alice.SlidingSync(t, sync3.Request{
			Lists: map[string]sync3.RequestList{
				"a": {
					Ranges: [][2]int64{{0, 20}},
					Filters: &sync3.RequestFilters{
						Spaces: spaces,
					},
				},
			},
		}, opts...)
		m.MatchResponse(t, res, m.MatchList("a", listMatchers...))
		return res
	}

	doInitialSpacesListRequest := func(spaces, wantRoomIDs []string) *sync3.Response {
		t.Helper()
		t.Logf("requesting initial rooms in spaces %v expecting %v", spaces, wantRoomIDs)
		return doSpacesListRequest(spaces, nil, m.MatchV3Count(len(wantRoomIDs)), m.MatchV3Ops(
			m.MatchV3SyncOp(
				0, int64(len(wantRoomIDs))-1, wantRoomIDs, true,
			),
		))
	}

	//  spaces[A] => B,C
	//  spaces[D] => E
	//  spaces[A,B] => B,C,E
	testCases := []struct {
		Spaces      []string
		WantRoomIDs []string
	}{
		{Spaces: []string{parentA}, WantRoomIDs: []string{roomB, roomC}},
		{Spaces: []string{parentD}, WantRoomIDs: []string{roomE}},
		{Spaces: []string{parentA, parentD}, WantRoomIDs: []string{roomB, roomC, roomE}},
	}
	for _, tc := range testCases {
		doInitialSpacesListRequest(tc.Spaces, tc.WantRoomIDs)
	}

	// now move F into D and re-query D
	alice.SendEventSynced(t, parentD, b.Event{
		Type:     "m.space.child",
		StateKey: &roomF,
		Content: map[string]interface{}{
			"via": []string{"example.com"},
		},
	})
	time.Sleep(100 * time.Millisecond) // let the proxy process this
	doInitialSpacesListRequest([]string{parentD}, []string{roomF, roomE})

	// now remove B and re-query A
	alice.SendEventSynced(t, parentA, b.Event{
		Type:     "m.space.child",
		StateKey: &roomB,
		Content:  map[string]interface{}{},
	})
	time.Sleep(100 * time.Millisecond) // let the proxy process this
	res := doInitialSpacesListRequest([]string{parentA}, []string{roomC})

	// now live stream an update to ensure it gets added
	alice.SendEventSynced(t, parentA, b.Event{
		Type:     "m.space.child",
		StateKey: &roomB,
		Content: map[string]interface{}{
			"via": []string{"example.com"},
		},
	})
	time.Sleep(100 * time.Millisecond) // let the proxy process this
	res = doSpacesListRequest([]string{parentA}, &res.Pos,
		m.MatchV3Count(2), m.MatchV3Ops(
			m.MatchV3DeleteOp(1),
			m.MatchV3InsertOp(1, roomB),
		),
	)

	// now completely change the space filter and ensure we see the right rooms
	doSpacesListRequest([]string{parentD}, &res.Pos,
		m.MatchV3Count(2), m.MatchV3Ops(
			m.MatchV3InvalidateOp(0, 1),
			m.MatchV3SyncOp(0, 1, []string{roomF, roomE}, true),
		),
	)
}

// Regression test for https://github.com/matrix-org/sliding-sync/issues/81 which has a list
// for invites EXCLUDING spaces, and yet space invites went into this list.
func TestSpacesFilterInvite(t *testing.T) {
	alice := registerNewUser(t)
	bob := registerNewUser(t)
	spaceRoomID := alice.MustCreateRoom(t, map[string]interface{}{
		"preset": "public_chat",
		"name":   "Space Room",
		"creation_content": map[string]string{
			"type": "m.space",
		},
	})
	normalRoomID := alice.MustCreateRoom(t, map[string]interface{}{
		"preset": "public_chat",
		"name":   "Normal Room",
	})
	t.Logf("Created space %v normal %v", spaceRoomID, normalRoomID)
	alice.InviteRoom(t, spaceRoomID, bob.UserID)
	alice.InviteRoom(t, normalRoomID, bob.UserID)
	// bob request invites for non-space rooms
	res := bob.SlidingSync(t, sync3.Request{
		Lists: map[string]sync3.RequestList{
			"a": {
				Ranges: sync3.SliceRanges{{0, 20}},
				Filters: &sync3.RequestFilters{
					IsInvite:     &boolTrue,
					NotRoomTypes: []*string{ptr("m.space")},
				},
				RoomSubscription: sync3.RoomSubscription{
					RequiredState: [][2]string{{"m.room.name", ""}},
				},
			},
		},
	})
	m.MatchResponse(t, res, m.MatchList("a", m.MatchV3Count(1), m.MatchV3Ops(
		m.MatchV3SyncOp(0, 0, []string{normalRoomID}),
	)))
}

// Regression test to catch https://github.com/matrix-org/sliding-sync/issues/85
func TestAddingUnknownChildToSpace(t *testing.T) {
	alice := registerNewUser(t)
	bob := registerNewUser(t)

	t.Log("Alice creates a space and invites Bob.")
	parentID := alice.MustCreateRoom(t, map[string]interface{}{
		"type":   "m.space",
		"invite": []string{bob.UserID},
	})

	t.Log("Bob accepts the invite.")
	bob.JoinRoom(t, parentID, nil)

	t.Log("Bob requests a new sliding sync.")
	res := bob.SlidingSync(t, sync3.Request{
		Lists: map[string]sync3.RequestList{
			"bob_list": {
				RoomSubscription: sync3.RoomSubscription{
					TimelineLimit: 10,
				},
				Ranges: sync3.SliceRanges{{0, 10}},
			},
		},
	})

	t.Log("Alice creates a room and marks it as a child of the space.")
	childID := alice.MustCreateRoom(t, map[string]interface{}{"preset": "public_chat"})
	childEventID := alice.Unsafe_SendEventUnsynced(t, parentID, b.Event{
		Type:     "m.space.child",
		StateKey: ptr(childID),
		Content: map[string]interface{}{
			"via": []string{"localhost"},
		},
	})

	t.Log("Bob syncs until he sees the m.space.child event in the space.")
	// Before the fix, this would panic inside getInitialRoomData, resulting in a 500
	res = bob.SlidingSyncUntilEventID(t, res.Pos, parentID, childEventID)
}
