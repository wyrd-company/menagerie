package main

import (
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/config"
)

func TestProvisioningClientsFollowTheSections(t *testing.T) {
	base := &config.Config{WyrwoodDirectory: "/run/sample"}
	clients, err := provisioningClients(base)
	if err != nil || clients.Bindings != nil || clients.Keys != nil {
		t.Fatalf("no section: %+v %v", clients, err)
	}

	wyrwood := &config.Wyrwood{Controller: "ssh://controller:2222", HostKey: "SHA256:" + strings.Repeat("a", 43), Agent: "sample-host", OperatorKey: "sample-key"}
	xolotl := &config.Xolotl{URL: "http://xolotl:8317", KeySecret: config.KeySecret{KVMount: "kv", Path: "sample/xolotl"}}
	cfg := *base
	cfg.Provisioning = &config.Provisioning{ReadinessWait: "5m", Wyrwood: wyrwood}
	clients, err = provisioningClients(&cfg)
	if err != nil || clients.Bindings == nil || clients.Keys != nil || clients.Wait != 5*time.Minute {
		t.Fatalf("wyrwood: %+v %v", clients, err)
	}
	cfg.Provisioning = &config.Provisioning{Xolotl: xolotl}
	clients, err = provisioningClients(&cfg)
	if err != nil || clients.Bindings != nil || clients.Keys == nil {
		t.Fatalf("xolotl: %+v %v", clients, err)
	}
	cfg.Provisioning = &config.Provisioning{Wyrwood: &config.Wyrwood{Controller: "not-a-controller"}}
	if _, err := provisioningClients(&cfg); err == nil {
		t.Fatal("an invalid section passed")
	}
}
