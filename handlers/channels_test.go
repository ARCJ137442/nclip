package handlers

import (
	"fmt"
	"testing"
	"time"

	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/channels"
)

func TestChannelCreateLimiterHasFixedStateBound(t *testing.T) {
	handler := NewChannelHandler(channels.NewManager(), &config.Config{}, nil)
	now := time.Now()
	handler.createMu.Lock()
	defer handler.createMu.Unlock()
	for i := 0; i < maxTrackedCreateIPs; i++ {
		ip := fmt.Sprintf("client-%d", i)
		if !handler.allowCreateLocked(ip, now) {
			t.Fatalf("first request from IP %d was denied", i)
		}
		handler.recordCreateLocked(ip, now)
	}
	if len(handler.createWindows) != maxTrackedCreateIPs {
		t.Fatalf("tracked IP count = %d", len(handler.createWindows))
	}
	if handler.allowCreateLocked("overflow-client", now) {
		t.Fatal("limiter accepted an IP beyond its fixed state bound")
	}
	if len(handler.createWindows) != maxTrackedCreateIPs {
		t.Fatalf("tracked IP state grew past bound: %d", len(handler.createWindows))
	}
}
