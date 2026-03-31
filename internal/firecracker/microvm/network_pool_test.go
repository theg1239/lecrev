package microvm

import (
	"context"
	"testing"
	"time"
)

func TestBuildTapPoolConfigs(t *testing.T) {
	t.Parallel()

	configs, err := buildTapPoolConfigs("tap", 3, "172.16.0.0/24", 30)
	if err != nil {
		t.Fatalf("build tap pool configs: %v", err)
	}
	if len(configs) != 3 {
		t.Fatalf("expected 3 tap configs, got %d", len(configs))
	}
	want := []networkConfig{
		{TapDevice: "tap0", GuestIP: "172.16.0.2", GatewayIP: "172.16.0.1", Netmask: "255.255.255.252", GuestMAC: "06:00:ac:10:00:02"},
		{TapDevice: "tap1", GuestIP: "172.16.0.6", GatewayIP: "172.16.0.5", Netmask: "255.255.255.252", GuestMAC: "06:00:ac:10:00:06"},
		{TapDevice: "tap2", GuestIP: "172.16.0.10", GatewayIP: "172.16.0.9", Netmask: "255.255.255.252", GuestMAC: "06:00:ac:10:00:0a"},
	}
	for i := range want {
		if configs[i] != want[i] {
			t.Fatalf("tap config %d mismatch: got %+v want %+v", i, configs[i], want[i])
		}
	}
}

func TestNetworkPoolAcquireRelease(t *testing.T) {
	t.Parallel()

	pool, err := newNetworkPool([]networkConfig{
		{TapDevice: "tap0", GuestIP: "172.16.0.2", GatewayIP: "172.16.0.1", Netmask: "255.255.255.252"},
		{TapDevice: "tap1", GuestIP: "172.16.0.6", GatewayIP: "172.16.0.5", Netmask: "255.255.255.252"},
	})
	if err != nil {
		t.Fatalf("new network pool: %v", err)
	}

	leaseA, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire lease a: %v", err)
	}
	leaseB, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire lease b: %v", err)
	}
	if leaseA.cfg.TapDevice == leaseB.cfg.TapDevice {
		t.Fatalf("expected unique leases, got %s twice", leaseA.cfg.TapDevice)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := pool.acquire(waitCtx); err == nil {
		t.Fatal("expected third acquire to block until timeout")
	}

	leaseA.Release()
	leaseC, err := pool.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire lease c after release: %v", err)
	}
	if leaseC.cfg.TapDevice != leaseA.cfg.TapDevice {
		t.Fatalf("expected released tap %s to be reusable, got %s", leaseA.cfg.TapDevice, leaseC.cfg.TapDevice)
	}
	leaseB.Release()
	leaseC.Release()
}

func TestNetworkPoolAcquireSpecificWaitsForRelease(t *testing.T) {
	t.Parallel()

	pool, err := newNetworkPool([]networkConfig{
		{TapDevice: "tap0", GuestIP: "172.16.0.2", GatewayIP: "172.16.0.1", Netmask: "255.255.255.252"},
		{TapDevice: "tap1", GuestIP: "172.16.0.6", GatewayIP: "172.16.0.5", Netmask: "255.255.255.252"},
	})
	if err != nil {
		t.Fatalf("new network pool: %v", err)
	}

	lease, err := pool.acquireSpecific(context.Background(), "tap0")
	if err != nil {
		t.Fatalf("acquire specific lease: %v", err)
	}

	released := make(chan struct{})
	go func() {
		time.Sleep(25 * time.Millisecond)
		lease.Release()
		close(released)
	}()

	waitCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	nextLease, err := pool.acquireSpecific(waitCtx, "tap0")
	if err != nil {
		t.Fatalf("reacquire specific lease after release: %v", err)
	}
	if nextLease.cfg.TapDevice != "tap0" {
		t.Fatalf("expected tap0 lease, got %s", nextLease.cfg.TapDevice)
	}
	<-released
	nextLease.Release()
}
