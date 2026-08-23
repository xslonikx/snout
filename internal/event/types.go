package event

// Consumer receives only the application's internal event types.
type Consumer interface {
	Consume(any)
}

type RouteAction string

const (
	RouteAdd    RouteAction = "add"
	RouteDelete RouteAction = "delete"
)

// EVPNRoute is the internal representation of an EVPN Type-2 route.
type EVPNRoute struct {
	RD   string `json:"rd"`
	VNI  uint32 `json:"vni"`
	MAC  string `json:"mac"`
	IP   string `json:"ip,omitempty"`
	VTEP string `json:"vtep"`
}

type RouteEvent struct {
	Action RouteAction `json:"action"`
	Route  EVPNRoute   `json:"route"`
}

// IMETRoute represents EVPN Type-3 membership of a VTEP in a VNI.
type IMETRoute struct {
	RD   string `json:"rd"`
	VNI  uint32 `json:"vni"`
	VTEP string `json:"vtep"`
}

type IMETEvent struct {
	Action RouteAction `json:"action"`
	Route  IMETRoute   `json:"route"`
}

type PeerEventType string

const (
	PeerUp   PeerEventType = "peer_up"
	PeerDown PeerEventType = "peer_down"
)

type PeerEvent struct {
	Event PeerEventType `json:"event"`
	Peer  string        `json:"peer"`
}
