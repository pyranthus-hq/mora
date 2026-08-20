package mora

import (
	"time"

	healthpkg "github.com/pyranthus-hq/mora/internal/health"
)

const producerAcquireTimeout = healthpkg.ProducerAcquireTimeout

func producerStore(cfg Config) healthpkg.ProducerStore {
	return healthpkg.ProducerStore{StateDir: cfg.StateDir}
}

func producerStatusPath(cfg Config) string   { return producerStore(cfg).StatusPath() }
func producerExpectedPath(cfg Config) string { return producerStore(cfg).ExpectedPath() }
func producerLockPath(cfg Config) string     { return producerStore(cfg).LockPath() }
func acquireProducerLock(cfg Config, now time.Time) (func(), error) {
	return producerStore(cfg).Acquire(now)
}
func mutateProducers(cfg Config, mutate func(map[string]producerStatus) error) error {
	return producerStore(cfg).MutateStatus(mutate)
}
func loadProducerStatus(cfg Config) (map[string]producerStatus, error) {
	return producerStore(cfg).LoadStatus()
}
func saveProducerStatus(cfg Config, m map[string]producerStatus) error {
	return producerStore(cfg).SaveStatus(m)
}
func loadExpectedProducers(cfg Config) (map[string]expectedProducer, error) {
	return producerStore(cfg).LoadExpected()
}
func saveExpectedProducers(cfg Config, m map[string]expectedProducer) error {
	return producerStore(cfg).SaveExpected(m)
}
