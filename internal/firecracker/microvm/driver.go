package microvm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/theg1239/lecrev/internal/firecracker"
)

type Config struct {
	FirecrackerBinary string
	JailerBinary      string
	KernelImagePath   string
	RootFSPath        string

	WorkspaceDir   string
	ChrootBaseDir  string
	UseJailer      bool
	GuestInitPath  string
	GuestVSockPort uint32
	GuestCIDStart  uint32

	VCPUCount       int64
	DefaultMemoryMB int64

	BootArgs string

	TapDevice string
	GuestMAC  string
	GuestIP   string
	GatewayIP string
	Netmask   string

	StartTimeout    time.Duration
	ConnectTimeout  time.Duration
	ShutdownTimeout time.Duration

	JailerUID int
	JailerGID int
}

type Driver struct {
	config  Config
	mu      sync.Mutex
	nextCID uint32
}

func New(cfg Config) (*Driver, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Driver{
		config:  cfg,
		nextCID: cfg.GuestCIDStart,
	}, nil
}

func (d *Driver) Name() string {
	return "firecracker"
}

func (d *Driver) Execute(ctx context.Context, req firecracker.ExecuteRequest) (*firecracker.ExecuteResult, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("firecracker driver requires linux hosts")
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fmt.Errorf("firecracker driver requires /dev/kvm: %w", err)
	}
	if req.MemoryMB <= 0 {
		req.MemoryMB = int(d.config.DefaultMemoryMB)
	}

	layout, vmID, err := d.prepareLayout(req.AttemptID)
	if err != nil {
		return nil, err
	}
	defer layout.cleanup()

	if err := stageKernel(layout.kernelPath, d.config.KernelImagePath); err != nil {
		return nil, err
	}
	if err := copyFile(d.config.RootFSPath, layout.rootFSPath); err != nil {
		return nil, err
	}
	if d.config.UseJailer {
		if err := os.Chown(layout.rootDir, d.config.JailerUID, d.config.JailerGID); err != nil && !errors.Is(err, os.ErrPermission) {
			return nil, err
		}
		for _, path := range []string{layout.kernelPath, layout.rootFSPath} {
			if err := os.Chown(path, d.config.JailerUID, d.config.JailerGID); err != nil && !errors.Is(err, os.ErrPermission) {
				return nil, err
			}
		}
	}

	proc, err := d.startVM(ctx, vmID, layout)
	if err != nil {
		return nil, err
	}
	defer proc.close()

	if err := waitForFile(ctx, layout.apiSocketHost, d.config.StartTimeout, proc); err != nil {
		return nil, fmt.Errorf("wait for firecracker api socket: %w; logs: %s", err, proc.logs())
	}

	api := newAPIClient(layout.apiSocketHost)
	guestCID := d.allocCID()
	bootArgs, err := d.bootArgs(req)
	if err != nil {
		return nil, err
	}
	if err := api.put(ctx, "/machine-config", machineConfig{
		VCPUCount:  d.config.VCPUCount,
		MemSizeMib: int64(req.MemoryMB),
		Smt:        false,
	}); err != nil {
		return nil, fmt.Errorf("configure machine: %w; logs: %s", err, proc.logs())
	}
	if err := api.put(ctx, "/boot-source", bootSource{
		KernelImagePath: layout.guestKernelPath,
		BootArgs:        bootArgs,
	}); err != nil {
		return nil, fmt.Errorf("configure boot source: %w; logs: %s", err, proc.logs())
	}
	if err := api.put(ctx, "/drives/rootfs", drive{
		DriveID:      "rootfs",
		PathOnHost:   layout.guestRootFSPath,
		IsRootDevice: true,
		IsReadOnly:   false,
	}); err != nil {
		return nil, fmt.Errorf("configure rootfs: %w; logs: %s", err, proc.logs())
	}
	if strings.EqualFold(req.NetworkPolicy, "full") {
		if err := api.put(ctx, "/network-interfaces/eth0", networkInterface{
			IfaceID:     "eth0",
			HostDevName: d.config.TapDevice,
			GuestMAC:    d.config.GuestMAC,
		}); err != nil {
			return nil, fmt.Errorf("configure network interface: %w; logs: %s", err, proc.logs())
		}
	}
	if err := api.put(ctx, "/vsock", vsockDevice{
		VsockID:  "vsock0",
		GuestCID: guestCID,
		UDSPath:  layout.guestVSockPath,
	}); err != nil {
		return nil, fmt.Errorf("configure vsock: %w; logs: %s", err, proc.logs())
	}
	if err := api.put(ctx, "/actions", action{ActionType: "InstanceStart"}); err != nil {
		return nil, fmt.Errorf("start vm: %w; logs: %s", err, proc.logs())
	}

	invokeCtx, cancel := context.WithTimeout(ctx, d.config.ConnectTimeout+req.Timeout)
	defer cancel()

	response, err := invokeGuest(invokeCtx, layout.vsockSocketHost, d.config.GuestVSockPort, firecracker.GuestInvocationRequest{
		AttemptID:      req.AttemptID,
		JobID:          req.JobID,
		FunctionID:     req.FunctionID,
		Entrypoint:     req.Entrypoint,
		ArtifactBundle: req.ArtifactBundle,
		Payload:        req.Payload,
		Env:            req.Env,
		TimeoutMillis:  req.Timeout.Milliseconds(),
		Region:         req.Region,
		HostID:         req.HostID,
	})

	_ = api.put(context.Background(), "/actions", action{ActionType: "SendCtrlAltDel"})
	_ = proc.wait(d.config.ShutdownTimeout)

	if response == nil {
		return nil, fmt.Errorf("guest execution failed: %w; firecracker logs: %s", err, proc.logs())
	}
	result := &firecracker.ExecuteResult{
		ExitCode:         response.ExitCode,
		Logs:             response.Logs,
		Output:           response.Output,
		SnapshotEligible: false,
		StartedAt:        response.StartedAt,
		FinishedAt:       response.FinishedAt,
	}
	if err != nil {
		result.Logs = combineLogs(response.Logs, proc.logs())
		return result, err
	}
	if response.Error != "" {
		result.Logs = combineLogs(response.Logs, proc.logs())
		return result, errors.New(response.Error)
	}
	return result, nil
}

func (d *Driver) allocCID() uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	cid := d.nextCID
	d.nextCID++
	if d.nextCID < d.config.GuestCIDStart {
		d.nextCID = d.config.GuestCIDStart
	}
	return cid
}

func (d *Driver) bootArgs(req firecracker.ExecuteRequest) (string, error) {
	parts := []string{
		"console=ttyS0",
		"reboot=k",
		"panic=1",
		"pci=off",
		"rw",
		fmt.Sprintf("init=%s", d.config.GuestInitPath),
		fmt.Sprintf("lecrev.vsock_port=%d", d.config.GuestVSockPort),
	}
	if strings.TrimSpace(d.config.BootArgs) != "" {
		parts = append(parts, strings.Fields(d.config.BootArgs)...)
	}
	if strings.EqualFold(req.NetworkPolicy, "full") {
		if strings.TrimSpace(d.config.TapDevice) == "" || strings.TrimSpace(d.config.GuestIP) == "" || strings.TrimSpace(d.config.GatewayIP) == "" || strings.TrimSpace(d.config.Netmask) == "" {
			return "", fmt.Errorf("networkPolicy=full requires tapDevice, guestIP, gatewayIP, and netmask")
		}
		parts = append(parts, fmt.Sprintf("ip=%s::%s:%s::eth0:off", d.config.GuestIP, d.config.GatewayIP, d.config.Netmask))
	}
	return strings.Join(parts, " "), nil
}

func (c Config) withDefaults() Config {
	if strings.TrimSpace(c.WorkspaceDir) == "" {
		c.WorkspaceDir = os.TempDir()
	}
	if strings.TrimSpace(c.ChrootBaseDir) == "" {
		c.ChrootBaseDir = filepath.Join(c.WorkspaceDir, "jailer")
	}
	if strings.TrimSpace(c.GuestInitPath) == "" {
		c.GuestInitPath = "/usr/local/bin/lecrev-guest-runner"
	}
	if c.GuestVSockPort == 0 {
		c.GuestVSockPort = 5005
	}
	if c.GuestCIDStart == 0 {
		c.GuestCIDStart = 3000
	}
	if c.VCPUCount == 0 {
		c.VCPUCount = 1
	}
	if c.DefaultMemoryMB == 0 {
		c.DefaultMemoryMB = 128
	}
	if c.StartTimeout == 0 {
		c.StartTimeout = 10 * time.Second
	}
	if c.ConnectTimeout == 0 {
		c.ConnectTimeout = 10 * time.Second
	}
	if c.ShutdownTimeout == 0 {
		c.ShutdownTimeout = 5 * time.Second
	}
	if c.UseJailer {
		if c.JailerUID == 0 {
			c.JailerUID = os.Getuid()
		}
		if c.JailerGID == 0 {
			c.JailerGID = os.Getgid()
		}
	}
	return c
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.FirecrackerBinary) == "" {
		return fmt.Errorf("firecracker binary is required")
	}
	if strings.TrimSpace(c.KernelImagePath) == "" {
		return fmt.Errorf("kernel image path is required")
	}
	if strings.TrimSpace(c.RootFSPath) == "" {
		return fmt.Errorf("rootfs path is required")
	}
	if c.UseJailer && strings.TrimSpace(c.JailerBinary) == "" {
		return fmt.Errorf("jailer binary is required when useJailer is enabled")
	}
	return nil
}

type vmLayout struct {
	rootDir         string
	apiSocketHost   string
	vsockSocketHost string
	kernelPath      string
	rootFSPath      string
	guestKernelPath string
	guestRootFSPath string
	guestVSockPath  string
	cleanup         func()
}

func (d *Driver) prepareLayout(attemptID string) (*vmLayout, string, error) {
	vmID := sanitizeID(attemptID)
	if vmID == "" {
		vmID = fmt.Sprintf("vm-%d", time.Now().UTC().UnixNano())
	}

	if d.config.UseJailer {
		vmRoot := filepath.Join(d.config.ChrootBaseDir, "firecracker", vmID, "root")
		if err := os.MkdirAll(vmRoot, 0o755); err != nil {
			return nil, "", err
		}
		return &vmLayout{
			rootDir:         vmRoot,
			apiSocketHost:   filepath.Join(vmRoot, "api.socket"),
			vsockSocketHost: filepath.Join(vmRoot, "vsock.socket"),
			kernelPath:      filepath.Join(vmRoot, "vmlinux"),
			rootFSPath:      filepath.Join(vmRoot, "rootfs.ext4"),
			guestKernelPath: "vmlinux",
			guestRootFSPath: "rootfs.ext4",
			guestVSockPath:  "vsock.socket",
			cleanup: func() {
				_ = os.RemoveAll(filepath.Join(d.config.ChrootBaseDir, "firecracker", vmID))
			},
		}, vmID, nil
	}

	rootDir, err := os.MkdirTemp(d.config.WorkspaceDir, "lecrev-firecracker-*")
	if err != nil {
		return nil, "", err
	}
	return &vmLayout{
		rootDir:         rootDir,
		apiSocketHost:   filepath.Join(rootDir, "api.socket"),
		vsockSocketHost: filepath.Join(rootDir, "vsock.socket"),
		kernelPath:      filepath.Join(rootDir, "vmlinux"),
		rootFSPath:      filepath.Join(rootDir, "rootfs.ext4"),
		guestKernelPath: "vmlinux",
		guestRootFSPath: "rootfs.ext4",
		guestVSockPath:  "vsock.socket",
		cleanup: func() {
			_ = os.RemoveAll(rootDir)
		},
	}, vmID, nil
}

type vmProcess struct {
	cmd     *exec.Cmd
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	waitCh  chan error
	mu      sync.Mutex
	done    bool
	waitErr error
}

func (d *Driver) startVM(ctx context.Context, vmID string, layout *vmLayout) (*vmProcess, error) {
	proc := &vmProcess{waitCh: make(chan error, 1)}
	if d.config.UseJailer {
		args := []string{
			"--id", vmID,
			"--exec-file", d.config.FirecrackerBinary,
			"--uid", strconv.Itoa(d.config.JailerUID),
			"--gid", strconv.Itoa(d.config.JailerGID),
			"--chroot-base-dir", d.config.ChrootBaseDir,
			"--",
			"--api-sock", "api.socket",
		}
		proc.cmd = exec.CommandContext(ctx, d.config.JailerBinary, args...)
	} else {
		proc.cmd = exec.CommandContext(ctx, d.config.FirecrackerBinary, "--api-sock", "api.socket")
		proc.cmd.Dir = layout.rootDir
	}
	proc.cmd.Stdout = &proc.stdout
	proc.cmd.Stderr = &proc.stderr
	if err := proc.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		proc.waitCh <- proc.cmd.Wait()
	}()
	return proc, nil
}

func (p *vmProcess) wait(timeout time.Duration) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.done {
		err := p.waitErr
		p.mu.Unlock()
		return err
	}
	p.mu.Unlock()
	select {
	case err := <-p.waitCh:
		p.markDone(err)
		return err
	case <-time.After(timeout):
		if p.cmd != nil && p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		select {
		case err := <-p.waitCh:
			p.markDone(err)
			return err
		case <-time.After(timeout):
			return fmt.Errorf("timed out waiting for firecracker process to exit")
		}
	}
}

func (p *vmProcess) close() error {
	return p.wait(2 * time.Second)
}

func (p *vmProcess) markDone(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done {
		return
	}
	p.done = true
	p.waitErr = err
}

func (p *vmProcess) logs() string {
	if p == nil {
		return ""
	}
	return combineLogs(strings.TrimSpace(p.stdout.String()), strings.TrimSpace(p.stderr.String()))
}

type apiClient struct {
	http *http.Client
}

func newAPIClient(socketPath string) *apiClient {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &apiClient{
		http: &http.Client{Transport: transport},
	}
}

func (c *apiClient) put(ctx context.Context, path string, body any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://firecracker"+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	raw, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("firecracker api %s returned status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(raw)))
}

type machineConfig struct {
	VCPUCount  int64 `json:"vcpu_count"`
	MemSizeMib int64 `json:"mem_size_mib"`
	Smt        bool  `json:"smt"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

type vsockDevice struct {
	VsockID  string `json:"vsock_id"`
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

type networkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

type action struct {
	ActionType string `json:"action_type"`
}

func invokeGuest(ctx context.Context, socketPath string, port uint32, request firecracker.GuestInvocationRequest) (*firecracker.GuestInvocationResponse, error) {
	return invokeGuestWithDialer(ctx, func(ctx context.Context) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socketPath)
	}, port, request)
}

func invokeGuestWithDialer(ctx context.Context, dial func(context.Context) (net.Conn, error), port uint32, request firecracker.GuestInvocationRequest) (*firecracker.GuestInvocationResponse, error) {
	var lastErr error
	deadline := time.Now().Add(10 * time.Second)
	if cut, ok := ctx.Deadline(); ok {
		deadline = cut
	}
	for time.Now().Before(deadline) {
		response, err := invokeGuestOnce(ctx, dial, port, request)
		if err == nil {
			return response, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out waiting for guest runner")
	}
	return nil, lastErr
}

func invokeGuestOnce(ctx context.Context, dial func(context.Context) (net.Conn, error), port uint32, request firecracker.GuestInvocationRequest) (*firecracker.GuestInvocationResponse, error) {
	conn, err := dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if _, err := io.WriteString(conn, fmt.Sprintf("CONNECT %d\n", port)); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "OK") {
		return nil, fmt.Errorf("unexpected vsock handshake response %q", strings.TrimSpace(line))
	}
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return nil, err
	}
	var response firecracker.GuestInvocationResponse
	if err := json.NewDecoder(reader).Decode(&response); err != nil {
		return nil, err
	}
	return &response, nil
}

func waitForFile(ctx context.Context, path string, timeout time.Duration, proc *vmProcess) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-proc.waitCh:
			proc.markDone(err)
			if err != nil {
				return err
			}
			return fmt.Errorf("process exited before %s appeared", path)
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func stageKernel(targetPath, sourcePath string) error {
	if err := os.Link(sourcePath, targetPath); err == nil {
		return nil
	}
	return copyFile(sourcePath, targetPath)
}

func copyFile(sourcePath, targetPath string) error {
	src, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return err
	}

	dst, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
}

func sanitizeID(input string) string {
	if input == "" {
		return ""
	}
	var builder strings.Builder
	for _, r := range input {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-', r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return builder.String()
}

func combineLogs(primary, secondary string) string {
	primary = strings.TrimSpace(primary)
	secondary = strings.TrimSpace(secondary)
	switch {
	case primary == "":
		return secondary
	case secondary == "":
		return primary
	default:
		return primary + "\n\n[firecracker]\n" + secondary
	}
}
