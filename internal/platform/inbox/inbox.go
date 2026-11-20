// Package inbox is the consumer inbox contract (D-23, concurrency.md §7.4).
//
// Delivery is at-least-once. Consumers are idempotent via (consumer, message_id).
// This is not exactly-once delivery. The package does not import pgx.
package inbox

const (
	// MaxConsumerLen matches ck_inbox_messages_consumer.
	MaxConsumerLen = 255
)
