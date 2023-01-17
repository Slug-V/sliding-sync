package caches

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"sync"

	"github.com/matrix-org/sliding-sync/internal"
	"github.com/matrix-org/sliding-sync/state"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
)

const PosAlwaysProcess = -2
const PosDoNotProcess = -1

type EventData struct {
	Event     json.RawMessage
	RoomID    string
	EventType string
	StateKey  *string
	Content   gjson.Result
	Timestamp uint64
	Sender    string

	// the number of joined users in this room. Use this value and don't try to work it out as you
	// may get it wrong due to Synapse sending duplicate join events(!) This value has them de-duped
	// correctly.
	JoinCount   int
	InviteCount int

	// the absolute latest position for this event data. The NID for this event is guaranteed to
	// be <= this value. See PosAlwaysProcess and PosDoNotProcess for things outside the event timeline
	// e.g invites
	LatestPos int64

	// Flag set when this event should force the room contents to be resent e.g
	// state res, initial join, etc
	ForceInitial bool
}

var logger = zerolog.New(os.Stdout).With().Timestamp().Logger().Output(zerolog.ConsoleWriter{
	Out:        os.Stderr,
	TimeFormat: "15:04:05",
})

// The purpose of global cache is to store global-level information about all rooms the server is aware of.
// Global-level information is represented as internal.RoomMetadata and includes things like Heroes, join/invite
// counts, if the room is encrypted, etc. Basically anything that is the same for all users of the system. This
// information is populated at startup from the database and then kept up-to-date by hooking into the
// Dispatcher for new events.
type GlobalCache struct {
	LoadJoinedRoomsOverride func(userID string) (pos int64, joinedRooms map[string]*internal.RoomMetadata, err error)

	// inserts are done by v2 poll loops, selects are done by v3 request threads
	// there are lots of overlapping keys as many users (threads) can be joined to the same room (key)
	// hence you must lock this with `mu` before r/w
	roomIDToMetadata   map[string]*internal.RoomMetadata
	roomIDToMetadataMu *sync.RWMutex

	// for loading room state not held in-memory TODO: remove to another struct along with associated functions
	store *state.Storage
}

func NewGlobalCache(store *state.Storage) *GlobalCache {
	return &GlobalCache{
		roomIDToMetadataMu: &sync.RWMutex{},
		store:              store,
		roomIDToMetadata:   make(map[string]*internal.RoomMetadata),
	}
}

func (c *GlobalCache) OnRegistered(_ int64) error {
	return nil
}

// Load the current room metadata for the given room IDs. Races unless you call this in a dispatcher loop.
// Always returns copies of the room metadata so ownership can be passed to other threads.
// Keeps the ordering of the room IDs given.
func (c *GlobalCache) LoadRooms(roomIDs ...string) map[string]*internal.RoomMetadata {
	c.roomIDToMetadataMu.RLock()
	defer c.roomIDToMetadataMu.RUnlock()
	result := make(map[string]*internal.RoomMetadata, len(roomIDs))
	for i := range roomIDs {
		roomID := roomIDs[i]
		sr := c.roomIDToMetadata[roomID]
		if sr == nil {
			logger.Error().Str("room", roomID).Msg("GlobalCache.LoadRoom: no metadata for this room")
			continue
		}
		srCopy := *sr
		// copy the heroes or else we may modify the same slice which would be bad :(
		srCopy.Heroes = make([]internal.Hero, len(sr.Heroes))
		for i := range sr.Heroes {
			srCopy.Heroes[i] = sr.Heroes[i]
		}
		result[roomID] = &srCopy
	}
	return result
}

// Load all current joined room metadata for the user given. Returns the absolute database position along
// with the results. TODO: remove with LoadRoomState?
func (c *GlobalCache) LoadJoinedRooms(userID string) (pos int64, joinedRooms map[string]*internal.RoomMetadata, err error) {
	if c.LoadJoinedRoomsOverride != nil {
		return c.LoadJoinedRoomsOverride(userID)
	}
	initialLoadPosition, err := c.store.LatestEventNID()
	if err != nil {
		return 0, nil, err
	}
	joinedRoomIDs, err := c.store.JoinedRoomsAfterPosition(userID, initialLoadPosition)
	if err != nil {
		return 0, nil, err
	}
	// TODO: no guarantee that this state is the same as latest unless called in a dispatcher loop
	rooms := c.LoadRooms(joinedRoomIDs...)
	return initialLoadPosition, rooms, nil
}

func (c *GlobalCache) LoadStateEvent(ctx context.Context, roomID string, loadPosition int64, evType, stateKey string) json.RawMessage {
	roomIDToStateEvents, err := c.store.RoomStateAfterEventPosition(ctx, []string{roomID}, loadPosition, map[string][]string{
		evType: {stateKey},
	})
	if err != nil {
		logger.Err(err).Str("room", roomID).Int64("pos", loadPosition).Msg("failed to load room state")
		return nil
	}
	events := roomIDToStateEvents[roomID]
	if len(events) > 0 {
		return events[0].JSON
	}
	return nil
}

// TODO: remove? Doesn't touch global cache fields
func (c *GlobalCache) LoadRoomState(ctx context.Context, roomIDs []string, loadPosition int64, requiredStateMap *internal.RequiredStateMap, roomToUsersInTimeline map[string][]string) (map[string][]json.RawMessage, []state.Event) {
	if c.store == nil {
		return nil, nil
	}
	if requiredStateMap.Empty() {
		return nil, nil
	}
	resultMap := make(map[string][]json.RawMessage, len(roomIDs))
	roomIDToStateEvents, err := c.store.RoomStateAfterEventPosition(ctx, roomIDs, loadPosition, requiredStateMap.QueryStateMap())
	if err != nil {
		logger.Err(err).Strs("rooms", roomIDs).Int64("pos", loadPosition).Msg("failed to load room state")
		return nil, nil
	}
	var stateNIDs []state.Event
	for roomID, stateEvents := range roomIDToStateEvents {
		var result []json.RawMessage
		for _, ev := range stateEvents {
			if requiredStateMap.Include(ev.Type, ev.StateKey) {
				result = append(result, ev.JSON)
				stateNIDs = append(stateNIDs, state.Event{
					NID:      ev.NID,
					StateKey: ev.StateKey,
					Type:     ev.Type,
					RoomID:   roomID,
				})
			} else if requiredStateMap.IsLazyLoading() {
				usersInTimeline := roomToUsersInTimeline[roomID]
				for _, userID := range usersInTimeline {
					if ev.StateKey == userID {
						result = append(result, ev.JSON)
						stateNIDs = append(stateNIDs, state.Event{
							NID:      ev.NID,
							StateKey: ev.StateKey,
							Type:     ev.Type,
							RoomID:   roomID,
						})
					}
				}
			}
		}
		resultMap[roomID] = result
	}
	// TODO: cache?
	return resultMap, stateNIDs
}

// Startup will populate the cache with the provided metadata.
// Must be called prior to starting any v2 pollers else this operation can race. Consider:
//   - V2 poll loop started early
//   - Join event arrives, NID=50
//   - PopulateGlobalCache loads the latest NID=50, processes this join event in the process
//   - OnNewEvents is called with the join event
//   - join event is processed twice.
func (c *GlobalCache) Startup(roomIDToMetadata map[string]internal.RoomMetadata) error {
	c.roomIDToMetadataMu.Lock()
	defer c.roomIDToMetadataMu.Unlock()
	// sort room IDs for ease of debugging and for determinism
	roomIDs := make([]string, len(roomIDToMetadata))
	i := 0
	for r := range roomIDToMetadata {
		roomIDs[i] = r
		i++
	}
	sort.Strings(roomIDs)
	for _, roomID := range roomIDs {
		metadata := roomIDToMetadata[roomID]
		internal.Assert("room ID is set", metadata.RoomID != "")
		internal.Assert("last message timestamp exists", metadata.LastMessageTimestamp > 1)
		c.roomIDToMetadata[roomID] = &metadata
	}
	return nil
}

// =================================================
// Listener function called by dispatcher below
// =================================================

func (c *GlobalCache) OnEphemeralEvent(roomID string, ephEvent json.RawMessage) {
	evType := gjson.ParseBytes(ephEvent).Get("type").Str
	c.roomIDToMetadataMu.Lock()
	defer c.roomIDToMetadataMu.Unlock()
	metadata := c.roomIDToMetadata[roomID]
	if metadata == nil {
		metadata = &internal.RoomMetadata{
			RoomID:          roomID,
			ChildSpaceRooms: make(map[string]struct{}),
		}
	}

	switch evType {
	case "m.typing":
		metadata.TypingEvent = ephEvent
	}
	c.roomIDToMetadata[roomID] = metadata
}

func (c *GlobalCache) OnNewEvent(
	ed *EventData,
) {
	// update global state
	c.roomIDToMetadataMu.Lock()
	defer c.roomIDToMetadataMu.Unlock()
	metadata := c.roomIDToMetadata[ed.RoomID]
	if metadata == nil {
		metadata = &internal.RoomMetadata{
			RoomID:          ed.RoomID,
			ChildSpaceRooms: make(map[string]struct{}),
		}
	}
	switch ed.EventType {
	case "m.room.name":
		if ed.StateKey != nil && *ed.StateKey == "" {
			metadata.NameEvent = ed.Content.Get("name").Str
		}
	case "m.room.encryption":
		if ed.StateKey != nil && *ed.StateKey == "" {
			metadata.Encrypted = true
		}
	case "m.room.tombstone":
		if ed.StateKey != nil && *ed.StateKey == "" {
			newRoomID := ed.Content.Get("replacement_room").Str
			if newRoomID == "" {
				metadata.UpgradedRoomID = nil
			} else {
				metadata.UpgradedRoomID = &newRoomID
			}
		}
	case "m.room.canonical_alias":
		if ed.StateKey != nil && *ed.StateKey == "" {
			metadata.CanonicalAlias = ed.Content.Get("alias").Str
		}
	case "m.room.create":
		if ed.StateKey != nil && *ed.StateKey == "" {
			roomType := ed.Content.Get("type")
			if roomType.Exists() && roomType.Type == gjson.String {
				metadata.RoomType = &roomType.Str
			}
			predecessorRoomID := ed.Content.Get("predecessor.room_id").Str
			if predecessorRoomID != "" {
				metadata.PredecessorRoomID = &predecessorRoomID
			}
		}
	case "m.space.child": // only track space child changes for now, not parents
		if ed.StateKey != nil {
			isDeleted := !ed.Content.Get("via").IsArray()
			if isDeleted {
				delete(metadata.ChildSpaceRooms, *ed.StateKey)
			} else {
				metadata.ChildSpaceRooms[*ed.StateKey] = struct{}{}
			}
		}
	case "m.room.member":
		if ed.StateKey != nil {
			membership := ed.Content.Get("membership").Str
			eventJSON := gjson.ParseBytes(ed.Event)
			if internal.IsMembershipChange(eventJSON) {
				metadata.JoinCount = ed.JoinCount
				metadata.InviteCount = ed.InviteCount
				if membership == "leave" || membership == "ban" {
					// remove this user as a hero
					metadata.RemoveHero(*ed.StateKey)
				}

				if membership == "join" && eventJSON.Get("unsigned.prev_content.membership").Str == "invite" {
					// invite -> join, retire any outstanding invites
					err := c.store.InvitesTable.RemoveInvite(*ed.StateKey, ed.RoomID)
					if err != nil {
						logger.Err(err).Str("user", *ed.StateKey).Str("room", ed.RoomID).Msg("failed to remove accepted invite")
					}
				}
			}
			if len(metadata.Heroes) < 6 && (membership == "join" || membership == "invite") {
				// try to find the existing hero e.g they changed their display name
				found := false
				for i := range metadata.Heroes {
					if metadata.Heroes[i].ID == *ed.StateKey {
						metadata.Heroes[i].Name = ed.Content.Get("displayname").Str
						found = true
						break
					}
				}
				if !found {
					metadata.Heroes = append(metadata.Heroes, internal.Hero{
						ID:   *ed.StateKey,
						Name: ed.Content.Get("displayname").Str,
					})
				}
			}
		}
	}
	metadata.LastMessageTimestamp = ed.Timestamp
	c.roomIDToMetadata[ed.RoomID] = metadata
}
