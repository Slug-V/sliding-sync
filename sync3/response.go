package sync3

import (
	"encoding/json"
	"strconv"

	"github.com/matrix-org/sliding-sync/sync3/extensions"
	"github.com/tidwall/gjson"
)

const (
	OpSync       = "SYNC"
	OpInvalidate = "INVALIDATE"
	OpInsert     = "INSERT"
	OpDelete     = "DELETE"
)

type Response struct {
	Lists map[string]ResponseList `json:"lists"`

	Rooms      map[string]Room     `json:"rooms"`
	Extensions extensions.Response `json:"extensions"`

	Pos   string `json:"pos"`
	TxnID string `json:"txn_id,omitempty"`
}

type ResponseList struct {
	Ops   []ResponseOp `json:"ops,omitempty"`
	Count int          `json:"count"`
}

func (r *Response) PosInt() int64 {
	p, _ := strconv.ParseInt(r.Pos, 10, 64)
	return p
}

func (r *Response) ListOps() int {
	num := 0
	for _, l := range r.Lists {
		if len(l.Ops) > 0 {
			num += len(l.Ops)
		}
	}
	return num
}

func (r *Response) RoomIDsToTimelineEventIDs() map[string][]string {
	includedRoomIDs := make(map[string][]string)
	for roomID := range r.Rooms {
		eventIDs := make([]string, len(r.Rooms[roomID].Timeline))
		for i := range eventIDs {
			eventIDs[i] = gjson.ParseBytes(r.Rooms[roomID].Timeline[i]).Get("event_id").Str
		}
		includedRoomIDs[roomID] = eventIDs
	}
	return includedRoomIDs
}

// Custom unmarshal so we can dynamically create the right ResponseOp for Ops
func (r *Response) UnmarshalJSON(b []byte) error {
	temporary := struct {
		Rooms map[string]Room `json:"rooms"`
		Lists map[string]struct {
			Ops   []json.RawMessage `json:"ops"`
			Count int               `json:"count"`
		} `json:"lists"`
		Extensions extensions.Response `json:"extensions"`

		Pos   string `json:"pos"`
		TxnID string `json:"txn_id,omitempty"`
	}{}
	if err := json.Unmarshal(b, &temporary); err != nil {
		return err
	}
	r.Rooms = temporary.Rooms
	r.Pos = temporary.Pos
	r.TxnID = temporary.TxnID
	r.Extensions = temporary.Extensions
	r.Lists = make(map[string]ResponseList, len(temporary.Lists))

	for listKey, l := range temporary.Lists {
		var list ResponseList
		list.Count = l.Count
		for _, op := range l.Ops {
			if gjson.GetBytes(op, "range").Exists() {
				var oper ResponseOpRange
				if err := json.Unmarshal(op, &oper); err != nil {
					return err
				}
				list.Ops = append(list.Ops, &oper)
			} else {
				var oper ResponseOpSingle
				if err := json.Unmarshal(op, &oper); err != nil {
					return err
				}
				list.Ops = append(list.Ops, &oper)
			}
		}
		r.Lists[listKey] = list
	}

	return nil
}

type ResponseOp interface {
	Op() string
	// which rooms are we giving data about
	IncludedRoomIDs() []string
}

type ResponseOpRange struct {
	Operation string   `json:"op"`
	Range     [2]int64 `json:"range,omitempty"`
	RoomIDs   []string `json:"room_ids,omitempty"`
}

func (r *ResponseOpRange) Op() string {
	return r.Operation
}
func (r *ResponseOpRange) IncludedRoomIDs() []string {
	if r.Op() == OpInvalidate {
		return nil // the rooms are being excluded
	}
	return r.RoomIDs
}

type ResponseOpSingle struct {
	Operation string `json:"op"`
	Index     *int   `json:"index,omitempty"` // 0 is a valid value, hence *int
	RoomID    string `json:"room_id,omitempty"`
}

func (r *ResponseOpSingle) Op() string {
	return r.Operation
}

func (r *ResponseOpSingle) IncludedRoomIDs() []string {
	if r.Op() == OpDelete || r.RoomID == "" {
		return nil // the room is being excluded
	}
	return []string{r.RoomID}
}
