package microvm

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"
)

type networkConfig struct {
	TapDevice string
	GuestMAC  string
	GuestIP   string
	GatewayIP string
	Netmask   string
}

type networkPool struct {
	mu     sync.Mutex
	order  []string
	cfgs   map[string]networkConfig
	inUse  map[string]bool
	next   int
}

type networkLease struct {
	cfg   networkConfig
	pool  *networkPool
	once  sync.Once
}

func newNetworkPool(configs []networkConfig) (*networkPool, error) {
	if len(configs) == 0 {
		return nil, nil
	}
	pool := &networkPool{
		order: make([]string, 0, len(configs)),
		cfgs:  make(map[string]networkConfig, len(configs)),
		inUse: make(map[string]bool, len(configs)),
	}
	for _, cfg := range configs {
		if strings.TrimSpace(cfg.TapDevice) == "" {
			return nil, fmt.Errorf("network pool entry missing tap device")
		}
		if strings.TrimSpace(cfg.GuestIP) == "" || strings.TrimSpace(cfg.GatewayIP) == "" || strings.TrimSpace(cfg.Netmask) == "" {
			return nil, fmt.Errorf("network pool entry %s missing guest network tuple", cfg.TapDevice)
		}
		tapDevice := strings.TrimSpace(cfg.TapDevice)
		pool.order = append(pool.order, tapDevice)
		pool.cfgs[tapDevice] = cfg
		pool.inUse[tapDevice] = false
	}
	return pool, nil
}

func (p *networkPool) acquire(ctx context.Context) (networkLease, error) {
	if p == nil {
		return networkLease{}, fmt.Errorf("full-network execution requires a configured tap pool")
	}
	for {
		p.mu.Lock()
		if cfg, ok := p.acquireNextLocked(); ok {
			p.mu.Unlock()
			return networkLease{cfg: cfg, pool: p}, nil
		}
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return networkLease{}, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (p *networkPool) acquireSpecific(ctx context.Context, tapDevice string) (networkLease, error) {
	if p == nil {
		return networkLease{}, fmt.Errorf("full-network execution requires a configured tap pool")
	}
	tapDevice = strings.TrimSpace(tapDevice)
	if tapDevice == "" {
		return networkLease{}, fmt.Errorf("tap device is required")
	}
	for {
		p.mu.Lock()
		cfg, exists := p.cfgs[tapDevice]
		if !exists {
			p.mu.Unlock()
			return networkLease{}, fmt.Errorf("unknown tap device %s", tapDevice)
		}
		if !p.inUse[tapDevice] {
			p.inUse[tapDevice] = true
			p.mu.Unlock()
			return networkLease{cfg: cfg, pool: p}, nil
		}
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return networkLease{}, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func (l *networkLease) Release() {
	if l == nil || l.pool == nil {
		return
	}
	l.once.Do(func() {
		l.pool.release(strings.TrimSpace(l.cfg.TapDevice))
	})
}

func (p *networkPool) acquireNextLocked() (networkConfig, bool) {
	if len(p.order) == 0 {
		return networkConfig{}, false
	}
	for offset := 0; offset < len(p.order); offset++ {
		index := (p.next + offset) % len(p.order)
		tapDevice := p.order[index]
		if p.inUse[tapDevice] {
			continue
		}
		p.inUse[tapDevice] = true
		p.next = (index + 1) % len(p.order)
		return p.cfgs[tapDevice], true
	}
	return networkConfig{}, false
}

func (p *networkPool) release(tapDevice string) {
	if p == nil || tapDevice == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.inUse[tapDevice]; exists {
		p.inUse[tapDevice] = false
	}
}

func (c Config) buildNetworkPool() (*networkPool, error) {
	configs, err := c.networkConfigs()
	if err != nil {
		return nil, err
	}
	return newNetworkPool(configs)
}

func (c Config) networkConfigs() ([]networkConfig, error) {
	if c.TapCount > 0 {
		return buildTapPoolConfigs(c.TapDevicePrefix, c.TapCount, c.TapNetworkCIDR, c.TapSubnetPrefix)
	}
	if strings.TrimSpace(c.TapDevice) == "" {
		return nil, nil
	}
	return []networkConfig{{
		TapDevice: strings.TrimSpace(c.TapDevice),
		GuestMAC:  strings.TrimSpace(c.GuestMAC),
		GuestIP:   strings.TrimSpace(c.GuestIP),
		GatewayIP: strings.TrimSpace(c.GatewayIP),
		Netmask:   strings.TrimSpace(c.Netmask),
	}}, nil
}

func buildTapPoolConfigs(devicePrefix string, count int, networkCIDR string, subnetPrefix int) ([]networkConfig, error) {
	if count <= 0 {
		return nil, nil
	}
	if strings.TrimSpace(devicePrefix) == "" {
		devicePrefix = "tap"
	}
	if subnetPrefix <= 0 {
		subnetPrefix = 30
	}
	prefix, err := netip.ParsePrefix(strings.TrimSpace(networkCIDR))
	if err != nil {
		return nil, fmt.Errorf("parse tap network cidr %q: %w", networkCIDR, err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("tap network cidr must be ipv4")
	}
	if subnetPrefix < prefix.Bits() || subnetPrefix > 30 {
		return nil, fmt.Errorf("tap subnet prefix must be between %d and 30", prefix.Bits())
	}

	blockSize := uint32(1) << uint32(32-subnetPrefix)
	base := ipv4ToUint32(prefix.Addr())
	networkSize := uint32(1) << uint32(32-prefix.Bits())
	required := uint32(count) * blockSize
	if required > networkSize {
		return nil, fmt.Errorf("tap network cidr %s does not have capacity for %d /%d subnets", prefix.String(), count, subnetPrefix)
	}

	netmask := prefixLengthToMask(subnetPrefix)
	configs := make([]networkConfig, 0, count)
	for i := 0; i < count; i++ {
		subnetBase := base + uint32(i)*blockSize
		gateway := uint32ToIPv4(subnetBase + 1)
		guest := uint32ToIPv4(subnetBase + 2)
		configs = append(configs, networkConfig{
			TapDevice: fmt.Sprintf("%s%d", devicePrefix, i),
			GuestMAC:  guestMACForIP(guest),
			GuestIP:   guest.String(),
			GatewayIP: gateway.String(),
			Netmask:   netmask,
		})
	}
	return configs, nil
}

func guestMACForIP(addr netip.Addr) string {
	if !addr.Is4() {
		return "06:00:00:00:00:02"
	}
	raw := addr.As4()
	return fmt.Sprintf("06:00:%02x:%02x:%02x:%02x", raw[0], raw[1], raw[2], raw[3])
}

func prefixLengthToMask(bits int) string {
	mask := uint32(0)
	if bits > 0 {
		mask = ^uint32(0) << uint32(32-bits)
	}
	return uint32ToIPv4(mask).String()
}

func ipv4ToUint32(addr netip.Addr) uint32 {
	raw := addr.As4()
	return uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3])
}

func uint32ToIPv4(value uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{
		byte(value >> 24),
		byte(value >> 16),
		byte(value >> 8),
		byte(value),
	})
}

func isFullNetworkPolicy(policy string) bool {
	return strings.EqualFold(strings.TrimSpace(policy), "full")
}
