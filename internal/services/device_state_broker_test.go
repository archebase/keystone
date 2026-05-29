// SPDX-FileCopyrightText: 2026 ArcheBase
//
// SPDX-License-Identifier: MulanPSL-2.0

package services

import "testing"

func TestDeviceStateBrokerUnsubscribeDoesNotCloseChannel(t *testing.T) {
	broker := NewDeviceStateBroker()
	events, unsubscribe := broker.Subscribe(1)

	unsubscribe()

	select {
	case _, ok := <-events:
		if !ok {
			t.Fatalf("unsubscribe closed subscriber channel; concurrent publish can panic on send")
		}
	default:
	}
}

func TestDeviceStateBrokerConcurrentPublishUnsubscribe(t *testing.T) {
	broker := NewDeviceStateBroker()

	for i := 0; i < 1000; i++ {
		_, unsubscribe := broker.Subscribe(1)
		done := make(chan struct{})
		go func() {
			defer close(done)
			broker.Publish("robot-001", DeviceStateEvent{"type": "recorder_state"})
		}()
		unsubscribe()
		<-done
	}
}
