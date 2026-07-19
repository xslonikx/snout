package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	api "github.com/osrg/gobgp/v4/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultGoBGPAddress = "127.0.0.1:50051"
	eventQueueSize      = 128
)

// Consumer получает только внутренние структуры приложения.
//
// Consumers не должны работать с GoBGP protobuf напрямую.
type Consumer interface {
	Consume(any)
}

// EventBus содержит два независимых потока:
//
//   - route events для dataplane и наблюдаемости;
//   - peer events только для наблюдаемости.
//
// OVS и FDB должны подписываться только на route events.
type EventBus struct {
	routeConsumers []Consumer
	peerConsumers  []Consumer
}

func NewEventBus() *EventBus {
	return &EventBus{}
}

func (b *EventBus) AddRouteConsumer(consumer Consumer) {
	if consumer == nil {
		return
	}

	b.routeConsumers = append(b.routeConsumers, consumer)
}

func (b *EventBus) AddPeerConsumer(consumer Consumer) {
	if consumer == nil {
		return
	}

	b.peerConsumers = append(b.peerConsumers, consumer)
}

func (b *EventBus) RunRouteQueue(
	ctx context.Context,
	events <-chan RouteEvent,
) {
	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-events:
			if !ok {
				return
			}

			for _, consumer := range b.routeConsumers {
				consumer.Consume(event)
			}
		}
	}
}

func (b *EventBus) RunPeerQueue(
	ctx context.Context,
	events <-chan PeerEvent,
) {
	for {
		select {
		case <-ctx.Done():
			return

		case event, ok := <-events:
			if !ok {
				return
			}

			for _, consumer := range b.peerConsumers {
				consumer.Consume(event)
			}
		}
	}
}

type RouteAction string

const (
	RouteAdd    RouteAction = "add"
	RouteDelete RouteAction = "delete"
)

// EVPNRoute — внутреннее представление EVPN Type-2.
//
// В текущей модели Godbox:
//   - Ethernet Tag используется как VNI;
//   - MAC хранится строкой;
//   - IP опционален;
//   - VTEP берётся из MP_REACH next_hops.
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

type PeerEventType string

const (
	PeerUp   PeerEventType = "peer_up"
	PeerDown PeerEventType = "peer_down"
)

type PeerEvent struct {
	Event PeerEventType `json:"event"`
	Peer  string        `json:"peer"`
}

// StdoutConsumer выводит события в JSON.
//
// Он не знает ничего о GoBGP protobuf и может принимать
// как RouteEvent, так и PeerEvent.
type StdoutConsumer struct{}

func (StdoutConsumer) Consume(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		log.Printf("marshal event: %v", err)
		return
	}

	fmt.Println(string(data))
}

// DecodeRoute преобразует GoBGP Path во внутренний RouteEvent.
//
// Поддерживается EVPN Type-2: MAC/IP Advertisement Route.
//
// Признак удаления находится непосредственно в path.IsWithdraw.
// Разбирать MP_UNREACH для withdraw-событий не требуется.
func DecodeRoute(path *api.Path) (*RouteEvent, error) {
	if path == nil {
		return nil, errors.New("nil path")
	}

	route, err := decodeEVPNNLRI(path.GetNlri())
	if err != nil {
		return nil, err
	}

	vtep, err := decodeVTEP(path.GetPattrs())
	if err != nil {
		return nil, err
	}

	route.VTEP = vtep

	action := RouteAdd
	if path.GetIsWithdraw() {
		action = RouteDelete
	}

	return &RouteEvent{
		Action: action,
		Route:  route,
	}, nil
}

// decodeEVPNNLRI читает типизированный api.NLRI.
//
// api.NLRI содержит oneof. Для EVPN Type-2 используется
// GetEvpnMacadv(), возвращающий EVPNMACIPAdvertisementRoute.
func decodeEVPNNLRI(nlri *api.NLRI) (EVPNRoute, error) {
	if nlri == nil {
		return EVPNRoute{}, errors.New("path has no NLRI")
	}

	macAdvertisement := nlri.GetEvpnMacadv()
	if macAdvertisement == nil {
		return EVPNRoute{}, errors.New(
			"unsupported NLRI: expected EVPN MAC/IP advertisement route",
		)
	}

	mac := macAdvertisement.GetMacAddress()
	if mac == "" {
		return EVPNRoute{}, errors.New(
			"EVPN MAC/IP advertisement route has no MAC address",
		)
	}

	rd, err := formatRouteDistinguisher(
		macAdvertisement.GetRd(),
	)
	if err != nil {
		return EVPNRoute{}, fmt.Errorf(
			"decode route distinguisher: %w",
			err,
		)
	}

	return EVPNRoute{
		RD:  rd,
		VNI: macAdvertisement.GetEthernetTag(),
		MAC: mac,
		IP:  macAdvertisement.GetIpAddress(),
	}, nil
}

// decodeVTEP ищет типизированный MP_REACH attribute
// и возвращает его первый next-hop.
//
// path.NeighborIp использовать нельзя: в RR-топологии это будет
// адрес BGP-соседа (route reflector), а не исходный VTEP.
func decodeVTEP(attributes []*api.Attribute) (string, error) {
	for _, attribute := range attributes {
		if attribute == nil {
			continue
		}

		mpReach := attribute.GetMpReach()
		if mpReach == nil {
			continue
		}

		nextHops := mpReach.GetNextHops()
		if len(nextHops) == 0 {
			return "", errors.New(
				"MP_REACH attribute has no next-hop",
			)
		}

		if nextHops[0] == "" {
			return "", errors.New(
				"MP_REACH attribute contains an empty next-hop",
			)
		}

		return nextHops[0], nil
	}

	return "", errors.New("MP_REACH attribute not found")
}

// formatRouteDistinguisher поддерживает три стандартных
// представления RD:
//
//   - 2-byte ASN : assigned;
//   - IPv4        : assigned;
//   - 4-byte ASN : assigned.
func formatRouteDistinguisher(
	rd *api.RouteDistinguisher,
) (string, error) {
	if rd == nil {
		return "", errors.New("route distinguisher is missing")
	}

	if value := rd.GetTwoOctetAsn(); value != nil {
		return fmt.Sprintf(
			"%d:%d",
			value.GetAdmin(),
			value.GetAssigned(),
		), nil
	}

	if value := rd.GetIpAddress(); value != nil {
		if value.GetAdmin() == "" {
			return "", errors.New(
				"route distinguisher has an empty IP administrator",
			)
		}

		return fmt.Sprintf(
			"%s:%d",
			value.GetAdmin(),
			value.GetAssigned(),
		), nil
	}

	if value := rd.GetFourOctetAsn(); value != nil {
		return fmt.Sprintf(
			"%d:%d",
			value.GetAdmin(),
			value.GetAssigned(),
		), nil
	}

	return "", errors.New(
		"unsupported route distinguisher type",
	)
}

// DecodePeer преобразует только реальные события смены состояния.
//
// TYPE_INIT и TYPE_END_OF_INIT служат для синхронизации watcher
// и не должны использоваться для управления dataplane.
func DecodePeer(
	event *api.WatchEventResponse_PeerEvent,
) (*PeerEvent, error) {
	if event == nil {
		return nil, errors.New("nil peer event")
	}

	if event.GetType() !=
		api.WatchEventResponse_PeerEvent_TYPE_STATE {
		return nil, nil
	}

	peer := event.GetPeer()
	if peer == nil {
		return nil, errors.New(
			"peer state event contains no peer",
		)
	}

	state := peer.GetState()
	if state == nil {
		return nil, errors.New(
			"peer state event contains no state",
		)
	}

	peerAddress := state.GetNeighborAddress()
	if peerAddress == "" {
		return nil, errors.New(
			"peer state event contains no neighbor address",
		)
	}

	eventType := PeerDown

	if state.GetSessionState() ==
		api.PeerState_SESSION_STATE_ESTABLISHED {
		eventType = PeerUp
	}

	return &PeerEvent{
		Event: eventType,
		Peer:  peerAddress,
	}, nil
}

// RunGoBGP запускает два независимых WatchEvent stream:
//
//   - EVPN table watcher;
//   - peer-state watcher.
//
// Watchers декодируют protobuf до помещения события в очередь.
// Поэтому consumers получают только структуры Godbox.
func RunGoBGP(
	ctx context.Context,
	client api.GoBgpServiceClient,
	routeEvents chan<- RouteEvent,
	peerEvents chan<- PeerEvent,
) error {
	errs := make(chan error, 2)

	go func() {
		errs <- watchTable(
			ctx,
			client,
			routeEvents,
		)
	}()

	go func() {
		errs <- watchPeers(
			ctx,
			client,
			peerEvents,
		)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()

	case err := <-errs:
		return err
	}
}

func watchTable(
	ctx context.Context,
	client api.GoBgpServiceClient,
	events chan<- RouteEvent,
) error {
	stream, err := client.WatchEvent(
		ctx,
		&api.WatchEventRequest{
			Table: &api.WatchEventRequest_Table{
				Filters: []*api.WatchEventRequest_Table_Filter{
					{
						Type: api.WatchEventRequest_Table_Filter_TYPE_BEST,
						Init: true,
					},
				},
			},
		},
	)
	if err != nil {
		return fmt.Errorf(
			"start table watcher: %w",
			err,
		)
	}

	for {
		response, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New(
					"table watcher reached EOF",
				)
			}

			if ctx.Err() != nil {
				return ctx.Err()
			}

			return fmt.Errorf(
				"receive table event: %w",
				err,
			)
		}

		table := response.GetTable()
		if table == nil {
			continue
		}

		// Один TableEvent может содержать несколько путей.
		// is_withdraw относится к каждому Path отдельно.
		for _, path := range table.GetPaths() {
			event, err := DecodeRoute(path)
			if err != nil {
				log.Printf(
					"skip EVPN path: %v",
					err,
				)
				continue
			}

			select {
			case events <- *event:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func watchPeers(
	ctx context.Context,
	client api.GoBgpServiceClient,
	events chan<- PeerEvent,
) error {
	stream, err := client.WatchEvent(
		ctx,
		&api.WatchEventRequest{
			Peer: &api.WatchEventRequest_Peer{},
		},
	)
	if err != nil {
		return fmt.Errorf(
			"start peer watcher: %w",
			err,
		)
	}

	for {
		response, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New(
					"peer watcher reached EOF",
				)
			}

			if ctx.Err() != nil {
				return ctx.Err()
			}

			return fmt.Errorf(
				"receive peer event: %w",
				err,
			)
		}

		peerEvent := response.GetPeer()
		if peerEvent == nil {
			continue
		}

		event, err := DecodePeer(peerEvent)
		if err != nil {
			log.Printf(
				"skip peer event: %v",
				err,
			)
			continue
		}

		// TYPE_INIT и TYPE_END_OF_INIT возвращают nil.
		if event == nil {
			continue
		}

		select {
		case events <- *event:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func main() {
	log.SetFlags(
		log.LstdFlags |
			log.Lmicroseconds |
			log.LUTC,
	)

	ctx, cancel := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)
	defer cancel()

	address := os.Getenv("GOBGP_GRPC_ADDRESS")
	if address == "" {
		address = defaultGoBGPAddress
	}

	conn, err := grpc.NewClient(
		address,
		grpc.WithTransportCredentials(
			insecure.NewCredentials(),
		),
	)
	if err != nil {
		log.Fatalf(
			"create GoBGP gRPC connection: %v",
			err,
		)
	}
	defer conn.Close()

	client := api.NewGoBgpServiceClient(conn)

	routeEvents := make(
		chan RouteEvent,
		eventQueueSize,
	)

	peerEvents := make(
		chan PeerEvent,
		eventQueueSize,
	)

	bus := NewEventBus()

	stdout := StdoutConsumer{}

	// stdout получает оба потока.
	bus.AddRouteConsumer(stdout)
	bus.AddPeerConsumer(stdout)

	// Dataplane consumers должны получать только RouteEvent:
	//
	// bus.AddRouteConsumer(ovsConsumer)
	// bus.AddRouteConsumer(fdbConsumer)

	go bus.RunRouteQueue(
		ctx,
		routeEvents,
	)

	go bus.RunPeerQueue(
		ctx,
		peerEvents,
	)

	log.Printf(
		"connecting to GoBGP gRPC at %s",
		address,
	)

	err = RunGoBGP(
		ctx,
		client,
		routeEvents,
		peerEvents,
	)

	if err != nil &&
		!errors.Is(err, context.Canceled) {
		log.Fatalf(
			"GoBGP watcher stopped: %v",
			err,
		)
	}
}
