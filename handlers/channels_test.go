package handlers

import (
	"fmt"
	"testing"

	"github.com/johnwmail/nclip/config"
	"github.com/johnwmail/nclip/internal/channels"
)

func TestChannelCreateLimiterHasFixedStateBound(t *testing.T) {
	handler := NewChannelHandler(channels.NewManager(), &config.Config{}, nil)
	for i := 0; i < maxTrackedCreateIPs; i++ {
		if !handler.allowCreate(fmt.Sprintf("client-%d", i)) {
			t.Fatalf("first request from IP %d was denied", i)
		}
	}
	if len(handler.createWindows) != maxTrackedCreateIPs {
		t.Fatalf("tracked IP count = %d", len(handler.createWindows))
	}
	if handler.allowCreate("overflow-client") {
		t.Fatal("limiter accepted an IP beyond its fixed state bound")
	}
	if len(handler.createWindows) != maxTrackedCreateIPs {
		t.Fatalf("tracked IP state grew past bound: %d", len(handler.createWindows))
	}
}
