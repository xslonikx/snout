package event

import "context"

// EventBus keeps route and peer event streams independent. Dataplane
// consumers should subscribe only to route events.
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

func (b *EventBus) RunRouteQueue(ctx context.Context, events <-chan RouteEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case routeEvent, ok := <-events:
			if !ok {
				return
			}

			for _, consumer := range b.routeConsumers {
				consumer.Consume(routeEvent)
			}
		}
	}
}

func (b *EventBus) RunPeerQueue(ctx context.Context, events <-chan PeerEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case peerEvent, ok := <-events:
			if !ok {
				return
			}

			for _, consumer := range b.peerConsumers {
				consumer.Consume(peerEvent)
			}
		}
	}
}
