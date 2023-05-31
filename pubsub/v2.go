package pubsub

import (
	"encoding/json"

	"github.com/matrix-org/sliding-sync/internal"
)

// The channel which has V2* payloads
const ChanV2 = "v2ch"

// V2Listener describes the messages that sync v2 pollers will publish.
type V2Listener interface {
	Initialise(p *V2Initialise)
	Accumulate(p *V2Accumulate)
	OnAccountData(p *V2AccountData)
	OnInvite(p *V2InviteRoom)
	OnLeftRoom(p *V2LeaveRoom)
	OnUnreadCounts(p *V2UnreadCounts)
	OnInitialSyncComplete(p *V2InitialSyncComplete)
	OnDeviceData(p *V2DeviceData)
	OnTyping(p *V2Typing)
	OnReceipt(p *V2Receipt)
	OnDeviceMessages(p *V2DeviceMessages)
	OnExpiredToken(p *V2ExpiredToken)
}

type V2Initialise struct {
	RoomID      string
	SnapshotNID int64
}

func (*V2Initialise) Type() string { return "V2Initialise" }

type V2Accumulate struct {
	RoomID    string
	PrevBatch string
	EventNIDs []int64
	// RoomReplacements is a slice of string pairs (old, new). Each entry corresponds
	// to a state event that the poller has just seen in the new room which marks the
	// new room as replacing the old room.
	RoomReplacements [][2]string
}

func (*V2Accumulate) Type() string { return "V2Accumulate" }

type V2UnreadCounts struct {
	UserID            string
	RoomID            string
	HighlightCount    *int
	NotificationCount *int
}

func (*V2UnreadCounts) Type() string { return "V2UnreadCounts" }

type V2AccountData struct {
	UserID string
	RoomID string
	Types  []string
}

func (*V2AccountData) Type() string { return "V2AccountData" }

type V2LeaveRoom struct {
	UserID string
	RoomID string
}

func (*V2LeaveRoom) Type() string { return "V2LeaveRoom" }

type V2InviteRoom struct {
	UserID string
	RoomID string
}

func (*V2InviteRoom) Type() string { return "V2InviteRoom" }

type V2InitialSyncComplete struct {
	UserID   string
	DeviceID string
}

func (*V2InitialSyncComplete) Type() string { return "V2InitialSyncComplete" }

type V2DeviceData struct {
	UserID   string
	DeviceID string
	Pos      int64
}

func (*V2DeviceData) Type() string { return "V2DeviceData" }

type V2Typing struct {
	RoomID         string
	EphemeralEvent json.RawMessage
}

func (*V2Typing) Type() string { return "V2Typing" }

type V2Receipt struct {
	RoomID   string
	Receipts []internal.Receipt
}

func (*V2Receipt) Type() string { return "V2Receipt" }

type V2DeviceMessages struct {
	UserID   string
	DeviceID string
}

func (*V2DeviceMessages) Type() string { return "V2DeviceMessages" }

type V2ExpiredToken struct {
	UserID   string
	DeviceID string
}

func (*V2ExpiredToken) Type() string { return "V2ExpiredToken" }

type V2Sub struct {
	listener Listener
	receiver V2Listener
}

func NewV2Sub(l Listener, recv V2Listener) *V2Sub {
	return &V2Sub{
		listener: l,
		receiver: recv,
	}
}

func (v *V2Sub) Teardown() {
	v.listener.Close()
}

func (v *V2Sub) onMessage(p Payload) {
	switch pl := p.(type) {
	case *V2Receipt:
		v.receiver.OnReceipt(pl)
	case *V2Initialise:
		v.receiver.Initialise(pl)
	case *V2Accumulate:
		v.receiver.Accumulate(pl)
	case *V2AccountData:
		v.receiver.OnAccountData(pl)
	case *V2InviteRoom:
		v.receiver.OnInvite(pl)
	case *V2LeaveRoom:
		v.receiver.OnLeftRoom(pl)
	case *V2UnreadCounts:
		v.receiver.OnUnreadCounts(pl)
	case *V2InitialSyncComplete:
		v.receiver.OnInitialSyncComplete(pl)
	case *V2DeviceData:
		v.receiver.OnDeviceData(pl)
	case *V2Typing:
		v.receiver.OnTyping(pl)
	case *V2DeviceMessages:
		v.receiver.OnDeviceMessages(pl)
	case *V2ExpiredToken:
		v.receiver.OnExpiredToken(pl)
	default:
		logger.Warn().Str("type", p.Type()).Msg("V2Sub: unhandled payload type")
	}
}

func (v *V2Sub) Listen() error {
	return v.listener.Listen(ChanV2, v.onMessage)
}
