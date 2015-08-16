package kafkaconsumer

import (
	"fmt"
	"time"

	"github.com/Shopify/sarama"
	"github.com/wvanbergen/kazoo-go"
	"gopkg.in/tomb.v1"
)

type partitionManager struct {
	parent    *consumerManager
	t         *tomb.Tomb
	partition *kazoo.Partition
}

// run implements the main partition manager loop.
// 1. Claim the partition in Zookeeper
// 2. Determine at what offset to start consuming
// 3. Start a consumer for the partition at the inital offset
// 4. Transfer messages and errors from the partition consumer to the consumer manager.
func (pm *partitionManager) run() {
	defer pm.t.Done()

	if err := pm.claimPartition(); err != nil {
		pm.t.Kill(err)
		return
	}
	defer pm.releasePartition()

	initialOffset, err := pm.fetchInitialOffset()
	if err != nil {
		pm.t.Kill(err)
		return
	}

	pc, err := pm.startPartitionConsumer(initialOffset)
	if err != nil {
		pm.t.Kill(err)
		return
	}
	defer pm.closePartitionConsumer(pc)

	for {
		select {
		case <-pm.t.Dying():
			return

		case msg := <-pc.Messages():
			select {
			case pm.parent.messages <- msg:
				// Track offset
			case <-pm.t.Dying():
				return
			}

		case err := <-pc.Errors():
			select {
			case pm.parent.errors <- err:
				// Noop?
			case <-pm.t.Dying():
				return
			}
		}
	}
}

// interrupt initiates the shutdown procedure for the partition manager, and returns immediately.
func (pm *partitionManager) interrupt() {
	pm.t.Kill(nil)
}

// close starts the shutdown proecure, and waits for it to complete.
func (pm *partitionManager) close() error {
	pm.interrupt()
	return pm.t.Wait()
}

// claimPartition claims a partition in Zookeeper for this instance.
// If the partition is already claimed by someone else, it will wait for the
// partition to become available. It will retry errors if they occur.
// This method should therefore only return with a nil error value, or
// tomb.ErrDying if the partitionManager was interrupted. Any other errors
// are not recoverable.
func (pm *partitionManager) claimPartition() error {
	sarama.Logger.Printf("Trying to claim partition %s...", pm.partition.Key())

	for {
		owner, changed, err := pm.parent.group.WatchPartitionOwner(pm.partition.Topic().Name, pm.partition.ID)
		if err != nil {
			sarama.Logger.Println("Failed to get partition owner from Zookeeper:", err)
			time.Sleep(1 * time.Second)
			continue
		}

		if owner != nil {
			if owner.ID == pm.parent.instance.ID {
				return fmt.Errorf("The current instance is already the owner of %s. This should not happen.", pm.partition.Key())
			}

			sarama.Logger.Printf("Partition %s is currently claimed by instance %s. Waiting for it to be released...", pm.partition.Key(), owner.ID)
			select {
			case <-changed:
				// The claim that is being watched was changed, let's try again!
				continue

			case <-pm.t.Dying():
				// The partition manager is shutting down, so let's bail
				return tomb.ErrDying
			}

		} else {

			err := pm.parent.instance.ClaimPartition(pm.partition.Topic().Name, pm.partition.ID)
			if err != nil {
				continue
			}

			sarama.Logger.Printf("Claimed ownership for %s", pm.partition.Key())
			return nil
		}
	}
}

// fetchInitialOffset determines the initial offset to use for the partition consumer.
// It will retry errors that occur. The returned error is nil if an offset was determined
// successfully, or tomb.ErrDying if the partition manager was interrupted. Any other
// error is non-recoverable.
func (pm *partitionManager) fetchInitialOffset() (int64, error) {
	for {
		select {
		case <-pm.t.Dying():
			return -1, tomb.ErrDying
		default:
		}

		initialOffset, err := pm.parent.group.FetchOffset(pm.partition.Topic().Name, pm.partition.ID)
		if err != nil {
			sarama.Logger.Printf("Failed to fetch offset for %s: %s\n", pm.partition.Key(), err)
			sarama.Logger.Printf("Trying again in 1 second...")
			time.Sleep(1 * time.Second)
			continue
		}

		// If the consumer never committed an offset for this partition before, a negative offset is returned.
		// In that case, we either start at the newest or oldest offset, based on the configuration.
		if initialOffset < 0 {
			initialOffset = pm.parent.config.Offsets.Initial
		}

		return initialOffset, nil
	}
}

// closePartitionConsumer starts a sarama consumer for the partition under management.
// This function will retry any error that may occur. The error return value is nil once
// it successfully has started the partition consumer, or tomb.ErrDying if the partition
// manager was interrupted. Any other error is not recoverable.
func (pm *partitionManager) startPartitionConsumer(initialOffset int64) (sarama.PartitionConsumer, error) {
	var (
		pc  sarama.PartitionConsumer
		err error
	)

	for {
		select {
		case <-pm.t.Dying():
			return nil, tomb.ErrDying
		default:
		}

		pc, err = pm.parent.consumer.ConsumePartition(pm.partition.Topic().Name, pm.partition.ID, initialOffset)
		switch err {
		case nil:
			switch initialOffset {
			case sarama.OffsetNewest:
				sarama.Logger.Printf("Started consumer for %s for new messages only.", pm.partition.Key())
			case sarama.OffsetOldest:
				sarama.Logger.Printf("Started consumer for %s at the oldest available offset.", pm.partition.Key())
			default:
				sarama.Logger.Printf("Started consumer for %s at offset %d.", pm.partition.Key(), initialOffset)
			}

			// We have a valid partition consumer so we can return
			return pc, nil

		case sarama.ErrOffsetOutOfRange:
			// The offset we had on file is too old. Restart with initial offset
			if pm.parent.config.Offsets.Initial == sarama.OffsetNewest {
				sarama.Logger.Printf("Offset %d is no longer available for %s. Trying again with new messages only...", initialOffset, pm.partition.Key())
			} else if pm.parent.config.Offsets.Initial == sarama.OffsetOldest {
				sarama.Logger.Printf("Offset %d is no longer available for %s. Trying again with he oldest available offset...", initialOffset, pm.partition.Key())
			}
			initialOffset = pm.parent.config.Offsets.Initial

			continue

		default:
			// Assume the problem is temporary; just try again.
			// FIXME: Do we want to treat all errors like this?
			// FIXME: Should te sleep by configurable?
			sarama.Logger.Printf("Failed to start consuming partition for %s: %s\n", pm.partition.Key(), err)
			sarama.Logger.Printf("Trying again in 1 second...")
			time.Sleep(1 * time.Second)

			continue
		}

	}
}

// closePartitionConsumer closes the sarama consumer for the partition under management.
func (pm *partitionManager) closePartitionConsumer(pc sarama.PartitionConsumer) {
	if err := pc.Close(); err != nil {
		sarama.Logger.Printf("Failed to close partition consumer for %s: %s\n", pm.partition.Key(), err)
	}
}

// releasePartition releases this instance's claim on this partition in Zookeeper.
func (pm *partitionManager) releasePartition() {
	if err := pm.parent.instance.ReleasePartition(pm.partition.Topic().Name, pm.partition.ID); err != nil {
		sarama.Logger.Printf("FAILED to release partition %s: %s", pm.partition.Key(), err)
	} else {
		sarama.Logger.Printf("Released partition %s.", pm.partition.Key())
	}
}
