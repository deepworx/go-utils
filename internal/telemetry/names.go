package telemetry

import "go.opentelemetry.io/otel/attribute"

// MaxLinks caps the links from a consumer batch span to the stored event
// contexts.
const MaxLinks = 128

// SpanCreate names the INTERNAL span of a Command that creates a Stream.
func SpanCreate(streamType string) string { return "create " + streamType }

// SpanUpdate names the INTERNAL span of a Command that updates a Stream.
func SpanUpdate(streamType string) string { return "update " + streamType }

// SpanSend names the PRODUCER span whose traceparent is stored on events.
func SpanSend(streamType string) string { return "send " + streamType }

// SpanProcess names the CONSUMER root span of a consumer batch.
func SpanProcess(consumer string) string { return "process " + consumer }

// SpanHandle names the INTERNAL span of one event within a batch.
func SpanHandle(eventType string) string { return "handle " + eventType }

// SpanInboxProcess names the CONSUMER span of one inbox message.
func SpanInboxProcess(source string) string { return "process inbox " + source }

// SpanSchedulerRun names the root span of one scheduler job run.
func SpanSchedulerRun(job string) string { return "scheduler.run " + job }

// Span and metric attribute keys. Metric attributes are limited to
// AttrConsumer, AttrJob, AttrSource and AttrStreamType, plus the bounded
// AttrOutcome and AttrStatus; tenants and IDs never become metric labels.
const (
	AttrTenantID   attribute.Key = "eventsourcing.tenant_id"
	AttrStreamType attribute.Key = "eventsourcing.stream_type"
	AttrStreamID   attribute.Key = "eventsourcing.stream_id"
	AttrEventID    attribute.Key = "eventsourcing.event_id"
	AttrEventType  attribute.Key = "eventsourcing.event_type"
	AttrEventTypes attribute.Key = "eventsourcing.event_types"
	AttrConsumer   attribute.Key = "eventsourcing.consumer"
	AttrPosition   attribute.Key = "eventsourcing.position"
	AttrBatchSize  attribute.Key = "eventsourcing.batch.size"
	AttrAttempt    attribute.Key = "eventsourcing.attempt"
	AttrJob        attribute.Key = "eventsourcing.job"
	AttrSource     attribute.Key = "eventsourcing.source"
	AttrOutcome    attribute.Key = "eventsourcing.outcome"
	AttrStatus     attribute.Key = "eventsourcing.status"
	AttrErrorType  attribute.Key = "error.type"
)

// AttributeKeys lists every registered attribute key.
var AttributeKeys = []attribute.Key{
	AttrTenantID, AttrStreamType, AttrStreamID, AttrEventID, AttrEventType,
	AttrEventTypes, AttrConsumer, AttrPosition, AttrBatchSize, AttrAttempt,
	AttrJob, AttrSource, AttrOutcome, AttrStatus, AttrErrorType,
}

// Log keys, the fixed slog vocabulary of every runner.
const (
	LogTenantID   = "tenant_id"
	LogConsumer   = "consumer"
	LogStreamType = "stream_type"
	LogStreamID   = "stream_id"
	LogEventID    = "event_id"
	LogEventType  = "event_type"
	LogPosition   = "position"
	LogAttempt    = "attempt"
	LogJob        = "job"
	LogSource     = "source"
	LogError      = "error"
)

// Detail log keys, used by single runners next to the fixed vocabulary.
const (
	LogMessageID = "message_id"
	LogOutcome   = "outcome"
	LogReason    = "reason"
	LogRetryAt   = "retry_at"
	LogStack     = "stack"
	LogCandidate = "candidate"
	LogReplayed  = "replayed"
	LogRemaining = "remaining"
	LogLease     = "lease"
)

// LogKeys lists every key of the fixed vocabulary.
var LogKeys = []string{
	LogTenantID, LogConsumer, LogStreamType, LogStreamID, LogEventID,
	LogEventType, LogPosition, LogAttempt, LogJob, LogSource, LogError,
}

// DurationBoundaries are the explicit buckets, in seconds, for duration
// histograms.
var DurationBoundaries = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300,
}

// EventCountBoundaries are the explicit buckets for event-count histograms.
var EventCountBoundaries = []float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 5000}

// Histograms.
var (
	AggregateLoadDuration = Instrument{
		Name: "eventsourcing.aggregate.load.duration", Unit: "s",
		Description: "Duration of loading an Aggregate from its Stream.",
		Boundaries:  DurationBoundaries,
	}
	AggregateLoadEvents = Instrument{
		Name: "eventsourcing.aggregate.load.events", Unit: "{event}",
		Description: "Events read to load an Aggregate.",
		Boundaries:  EventCountBoundaries,
	}
	CommandDuration = Instrument{
		Name: "eventsourcing.command.duration", Unit: "s",
		Description: "Duration of a Command transaction.",
		Boundaries:  DurationBoundaries,
	}
	ConsumerBatchDuration = Instrument{
		Name: "eventsourcing.consumer.batch.duration", Unit: "s",
		Description: "Duration of one Consumer batch.",
		Boundaries:  DurationBoundaries,
	}
	ConsumerProcessDuration = Instrument{
		Name: "eventsourcing.consumer.process.duration", Unit: "s",
		Description: "Duration of handling one event in a Consumer.",
		Boundaries:  DurationBoundaries,
	}
	InboxHandleDuration = Instrument{
		Name: "inbox.handle.duration", Unit: "s",
		Description: "Duration of handling one inbox message.",
		Boundaries:  DurationBoundaries,
	}
	SchedulerRunDuration = Instrument{
		Name: "scheduler.run.duration", Unit: "s",
		Description: "Duration of one scheduler job run.",
		Boundaries:  DurationBoundaries,
	}
)

// Counters and up-down counters.
var (
	InboxMessagesHandled = Instrument{
		Name: "inbox.messages.handled", Unit: "{message}",
		Description: "Inbox messages handled, by outcome.",
	}
	SchedulerRunCandidateFailures = Instrument{
		Name: "scheduler.run.candidate_failures", Unit: "{failure}",
		Description: "Failed runs of a scheduler job candidate.",
	}
	NotifyReconnects = Instrument{
		Name: "notify.reconnects", Unit: "{reconnect}",
		Description: "Reconnects of the LISTEN connection.",
	}
	NotifyGaps = Instrument{
		Name: "notify.gaps", Unit: "{gap}",
		Description: "Gaps delivered to subscriptions after overflow or reconnect.",
	}
	LeaseReconnects = Instrument{
		Name: "lease.reconnects", Unit: "{reconnect}",
		Description: "Reconnects of the advisory-lock connection.",
	}
	LeaseLost = Instrument{
		Name: "lease.lost", Unit: "{lease}",
		Description: "Leases lost while held.",
	}
	LeaseHeld = Instrument{
		Name: "lease.held", Unit: "{lease}",
		Description: "Leases currently held.",
	}
)

// Polled gauges, reported only by the lock or lease owner, except XminAge,
// which each process reports once.
var (
	ConsumerPending = Instrument{
		Name: "eventsourcing.consumer.pending", Unit: "{event}",
		Description: "Matching events after the Consumer's Cursor.",
	}
	ConsumerLag = Instrument{
		Name: "eventsourcing.consumer.lag", Unit: "s",
		Description: "Age of the oldest pending event of the Consumer.",
	}
	ConsumerStopped = Instrument{
		Name: "eventsourcing.consumer.stopped", Unit: "1",
		Description: "1 when the Consumer is Stopped, else 0.",
	}
	ReactorParkedStreams = Instrument{
		Name: "eventsourcing.reactor.parked.streams", Unit: "{stream}",
		Description: "Parked Streams of the Reactor.",
	}
	ReactorParkedEvents = Instrument{
		Name: "eventsourcing.reactor.parked.events", Unit: "{event}",
		Description: "Events held for Parked Streams of the Reactor.",
	}
	ReactorParkedExhausted = Instrument{
		Name: "eventsourcing.reactor.parked.exhausted", Unit: "{stream}",
		Description: "Parked Streams whose auto-redrive schedule is exhausted.",
	}
	InboxMessages = Instrument{
		Name: "inbox.messages", Unit: "{message}",
		Description: "Inbox messages, by status.",
	}
	InboxPendingOldestAge = Instrument{
		Name: "inbox.pending.oldest_age", Unit: "s",
		Description: "Age of the oldest pending inbox message.",
	}
	SchedulerJobLastSuccess = Instrument{
		Name: "scheduler.job.last_success", Unit: "s",
		Description: "Unix time of the job's last successful run.",
	}
	XminAge = Instrument{
		Name: "eventsourcing.xmin.age", Unit: "{transaction}",
		Description: "Age of the database's oldest running transaction ID.",
	}
)

// Instruments lists every registered instrument.
var Instruments = []Instrument{
	AggregateLoadDuration, AggregateLoadEvents, CommandDuration,
	ConsumerBatchDuration, ConsumerProcessDuration, InboxHandleDuration,
	SchedulerRunDuration,
	InboxMessagesHandled, SchedulerRunCandidateFailures, NotifyReconnects,
	NotifyGaps, LeaseReconnects, LeaseLost, LeaseHeld,
	ConsumerPending, ConsumerLag, ConsumerStopped, ReactorParkedStreams,
	ReactorParkedEvents, ReactorParkedExhausted, InboxMessages,
	InboxPendingOldestAge, SchedulerJobLastSuccess, XminAge,
}
