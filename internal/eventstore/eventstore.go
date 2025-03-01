package eventstore

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/zitadel/zitadel/internal/api/authz"
)

// Eventstore abstracts all functions needed to store valid events
// and filters the stored events
type Eventstore struct {
	PushTimeout time.Duration

	pusher  Pusher
	querier Querier

	instances         []string
	lastInstanceQuery time.Time
	instancesMu       sync.Mutex
}

var (
	eventInterceptors map[EventType]eventTypeInterceptors
	eventTypes        []string
	aggregateTypes    []string
)

// RegisterFilterEventMapper registers a function for mapping an eventstore event to an event
func RegisterFilterEventMapper(aggregateType AggregateType, eventType EventType, mapper func(Event) (Event, error)) {
	if mapper == nil || eventType == "" {
		return
	}

	appendEventType(eventType)
	appendAggregateType(aggregateType)

	if eventInterceptors == nil {
		eventInterceptors = make(map[EventType]eventTypeInterceptors)
	}
	interceptor := eventInterceptors[eventType]
	interceptor.eventMapper = mapper
	eventInterceptors[eventType] = interceptor
}

type eventTypeInterceptors struct {
	eventMapper func(Event) (Event, error)
}

func NewEventstore(config *Config) *Eventstore {
	return &Eventstore{
		PushTimeout: config.PushTimeout,

		pusher:  config.Pusher,
		querier: config.Querier,

		instancesMu: sync.Mutex{},
	}
}

// Health checks if the eventstore can properly work
// It checks if the repository can serve load
func (es *Eventstore) Health(ctx context.Context) error {
	if err := es.pusher.Health(ctx); err != nil {
		return err
	}
	return es.querier.Health(ctx)
}

// Push pushes the events in a single transaction
// an event needs at least an aggregate
func (es *Eventstore) Push(ctx context.Context, cmds ...Command) ([]Event, error) {
	if es.PushTimeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, es.PushTimeout)
		defer cancel()
	}
	events, err := es.pusher.Push(ctx, cmds...)
	if err != nil {
		return nil, err
	}

	mappedEvents, err := es.mapEvents(events)
	if err != nil {
		return mappedEvents, err
	}
	es.notify(mappedEvents)
	return mappedEvents, nil
}

func (es *Eventstore) EventTypes() []string {
	return eventTypes
}

func (es *Eventstore) AggregateTypes() []string {
	return aggregateTypes
}

// Filter filters the stored events based on the searchQuery
// and maps the events to the defined event structs
//
// Deprecated: Use [FilterToQueryReducer] instead to avoid allocations.
func (es *Eventstore) Filter(ctx context.Context, searchQuery *SearchQueryBuilder) ([]Event, error) {
	events := make([]Event, 0, searchQuery.GetLimit())
	searchQuery.ensureInstanceID(ctx)
	err := es.querier.FilterToReducer(ctx, searchQuery, func(event Event) error {
		event, err := es.mapEvent(event)
		if err != nil {
			return err
		}
		events = append(events, event)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

func (es *Eventstore) mapEvents(events []Event) (mappedEvents []Event, err error) {
	mappedEvents = make([]Event, len(events))
	for i, event := range events {
		mappedEvents[i], err = es.mapEventLocked(event)
		if err != nil {
			return nil, err
		}
	}
	return mappedEvents, nil
}

func (es *Eventstore) mapEvent(event Event) (Event, error) {
	return es.mapEventLocked(event)
}

func (es *Eventstore) mapEventLocked(event Event) (Event, error) {
	interceptors, ok := eventInterceptors[event.Type()]
	if !ok || interceptors.eventMapper == nil {
		return BaseEventFromRepo(event), nil
	}
	return interceptors.eventMapper(event)
}

// TODO: refactor so we can change to the following interface:
/*
type reducer interface {
	// Reduce applies an event on the object.
	Reduce(Event) error
}
*/

type reducer interface {
	//Reduce handles the events of the internal events list
	// it only appends the newly added events
	Reduce() error
	//AppendEvents appends the passed events to an internal list of events
	AppendEvents(...Event)
}

// FilterToReducer filters the events based on the search query, appends all events to the reducer and calls it's reduce function
func (es *Eventstore) FilterToReducer(ctx context.Context, searchQuery *SearchQueryBuilder, r reducer) error {
	searchQuery.ensureInstanceID(ctx)
	return es.querier.FilterToReducer(ctx, searchQuery, func(event Event) error {
		event, err := es.mapEvent(event)
		if err != nil {
			return err
		}
		r.AppendEvents(event)
		return r.Reduce()
	})
}

// LatestSequence filters the latest sequence for the given search query
func (es *Eventstore) LatestSequence(ctx context.Context, queryFactory *SearchQueryBuilder) (float64, error) {
	queryFactory.InstanceID(authz.GetInstance(ctx).InstanceID())

	return es.querier.LatestSequence(ctx, queryFactory)
}

// InstanceIDs returns the instance ids found by the search query
// forceDBCall forces to query the database, the instance ids are not cached
func (es *Eventstore) InstanceIDs(ctx context.Context, maxAge time.Duration, forceDBCall bool, queryFactory *SearchQueryBuilder) ([]string, error) {
	es.instancesMu.Lock()
	defer es.instancesMu.Unlock()

	if !forceDBCall && time.Since(es.lastInstanceQuery) <= maxAge {
		return es.instances, nil
	}

	instances, err := es.querier.InstanceIDs(ctx, queryFactory)
	if err != nil {
		return nil, err
	}

	if !forceDBCall {
		es.instances = instances
		es.lastInstanceQuery = time.Now()
	}

	return instances, nil
}

type QueryReducer interface {
	reducer
	//Query returns the SearchQueryFactory for the events needed in reducer
	Query() *SearchQueryBuilder
}

// FilterToQueryReducer filters the events based on the search query of the query function,
// appends all events to the reducer and calls it's reduce function
func (es *Eventstore) FilterToQueryReducer(ctx context.Context, r QueryReducer) error {
	return es.FilterToReducer(ctx, r.Query(), r)
}

type Reducer func(event Event) error

type Querier interface {
	// Health checks if the connection to the storage is available
	Health(ctx context.Context) error
	// FilterToReducer calls r for every event returned from the storage
	FilterToReducer(ctx context.Context, searchQuery *SearchQueryBuilder, r Reducer) error
	// LatestSequence returns the latest sequence found by the search query
	LatestSequence(ctx context.Context, queryFactory *SearchQueryBuilder) (float64, error)
	// InstanceIDs returns the instance ids found by the search query
	InstanceIDs(ctx context.Context, queryFactory *SearchQueryBuilder) ([]string, error)
}

type Pusher interface {
	// Health checks if the connection to the storage is available
	Health(ctx context.Context) error
	// Push stores the actions
	Push(ctx context.Context, commands ...Command) (_ []Event, err error)
}

func appendEventType(typ EventType) {
	i := sort.SearchStrings(eventTypes, string(typ))
	if i < len(eventTypes) && eventTypes[i] == string(typ) {
		return
	}
	eventTypes = append(eventTypes[:i], append([]string{string(typ)}, eventTypes[i:]...)...)
}

func appendAggregateType(typ AggregateType) {
	i := sort.SearchStrings(aggregateTypes, string(typ))
	if len(aggregateTypes) > i && aggregateTypes[i] == string(typ) {
		return
	}
	aggregateTypes = append(aggregateTypes[:i], append([]string{string(typ)}, aggregateTypes[i:]...)...)
}
