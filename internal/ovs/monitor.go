package ovs

import (
	"context"
	"fmt"
	"time"

	"github.com/xslonikx/snout/internal/event"
	"github.com/xslonikx/snout/internal/logging"
)

type Announcer interface {
	Announce(context.Context, event.EVPNRoute) error
	Withdraw(context.Context, event.EVPNRoute) error
}

type MonitorConfig struct {
	RD       string
	VNI      uint32
	VTEP     string
	Interval time.Duration
}

func MonitorLocalMACs(ctx context.Context, ovs OpenVSwitch, announcer Announcer, config MonitorConfig) error {
	if ovs == nil || announcer == nil {
		return fmt.Errorf("OVS and announcer are required")
	}
	if config.RD == "" || config.VNI == 0 || config.VTEP == "" {
		return fmt.Errorf("RD, VNI, and VTEP are required")
	}
	if config.Interval <= 0 {
		return fmt.Errorf("monitor interval must be positive")
	}

	known := make(map[string]event.EVPNRoute)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for mac, route := range known {
			if err := announcer.Withdraw(shutdownCtx, route); err != nil {
				logging.Errorf("withdraw local MAC %s during shutdown: %v", mac, err)
			}
		}
	}()
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()

	for {
		entries, err := ovs.LocalMACs(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logging.Errorf("read local OVS FDB: %v", err)
		} else {
			current := make(map[string]event.EVPNRoute, len(entries))
			for _, entry := range entries {
				route := event.EVPNRoute{RD: config.RD, VNI: config.VNI, MAC: entry.MAC, VTEP: config.VTEP}
				current[entry.MAC] = route
				if _, exists := known[entry.MAC]; !exists {
					if err := announcer.Announce(ctx, route); err != nil {
						logging.Errorf("announce local MAC %s: %v", entry.MAC, err)
						delete(current, entry.MAC)
					}
				}
			}
			for mac, route := range known {
				if _, exists := current[mac]; exists {
					continue
				}
				if err := announcer.Withdraw(ctx, route); err != nil {
					logging.Errorf("withdraw local MAC %s: %v", mac, err)
					current[mac] = route
				}
			}
			known = current
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
