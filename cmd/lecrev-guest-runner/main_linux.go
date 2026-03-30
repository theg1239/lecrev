//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/theg1239/lecrev/internal/firecracker"
	"github.com/theg1239/lecrev/internal/runtime/nodeexec"
)

const preparedRoot = "/var/lib/lecrev/functions"
const guestNodeBinary = "/usr/local/bin/node"
const preparedWorkerStartupTimeout = 10 * time.Second

func main() {
	if err := mountGuestFilesystems(); err != nil {
		log.Printf("guest runner mount setup failed: %v", err)
	}
	port, err := guestPort()
	if err != nil {
		log.Fatal(err)
	}
	if err := serve(port); err != nil {
		log.Fatal(err)
	}
}

func guestPort() (uint32, error) {
	if raw := strings.TrimSpace(os.Getenv("LECREV_VSOCK_PORT")); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return 0, fmt.Errorf("parse LECREV_VSOCK_PORT: %w", err)
		}
		return uint32(value), nil
	}

	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return 0, fmt.Errorf("read /proc/cmdline: %w", err)
	}
	for _, token := range strings.Fields(string(cmdline)) {
		if value, ok := strings.CutPrefix(token, "lecrev.vsock_port="); ok {
			parsed, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return 0, fmt.Errorf("parse lecrev.vsock_port: %w", err)
			}
			return uint32(parsed), nil
		}
	}
	return 0, fmt.Errorf("missing lecrev.vsock_port kernel parameter")
}

func mountGuestFilesystems() error {
	for _, path := range []string{"/dev", "/dev/pts", "/dev/shm", "/proc", "/sys", "/tmp", preparedRoot} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
	}
	if err := unix.Mount("devtmpfs", "/dev", "devtmpfs", 0, "mode=0755"); err != nil && !errors.Is(err, syscall.EBUSY) {
		return err
	}
	if err := unix.Mount("devpts", "/dev/pts", "devpts", 0, "newinstance,ptmxmode=0666,mode=0620"); err != nil && !errors.Is(err, syscall.EBUSY) {
		return err
	}
	if err := unix.Mount("tmpfs", "/dev/shm", "tmpfs", 0, "mode=1777"); err != nil && !errors.Is(err, syscall.EBUSY) {
		return err
	}
	if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
		return err
	}
	if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil && !errors.Is(err, syscall.EBUSY) {
		return err
	}
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", 0, "mode=1777"); err != nil && !errors.Is(err, syscall.EBUSY) {
		return err
	}
	return nil
}

func serve(port uint32) error {
	listener, err := listenVsock(port)
	if err != nil {
		return err
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		shouldShutdown, err := handleConnection(conn)
		_ = conn.Close()
		if err != nil {
			return err
		}
		if shouldShutdown {
			time.Sleep(250 * time.Millisecond)
			_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
			return nil
		}
	}
}

func handleConnection(conn io.ReadWriteCloser) (bool, error) {
	var request firecracker.GuestRequest
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		return false, err
	}

	var response firecracker.GuestResponse
	shouldShutdown := false
	switch request.Action {
	case firecracker.GuestActionPing:
		response.Ping = &firecracker.GuestPingResponse{Ready: true}
	case firecracker.GuestActionPrepare:
		prepareResp, err := prepare(request.Prepare)
		if err != nil {
			response.Error = err.Error()
		}
		response.Prepare = prepareResp
	case firecracker.GuestActionExecute:
		invokeResp, err := execute(request.Invocation)
		if err != nil {
			invokeResp.Error = err.Error()
		}
		response.Invocation = invokeResp
		shouldShutdown = true
	default:
		response.Error = fmt.Sprintf("unsupported guest action %q", request.Action)
	}

	if err := json.NewEncoder(conn).Encode(&response); err != nil {
		return false, err
	}
	return shouldShutdown, nil
}

func prepare(request *firecracker.GuestPrepareRequest) (*firecracker.GuestPrepareResponse, error) {
	if request == nil {
		return &firecracker.GuestPrepareResponse{Prepared: false}, fmt.Errorf("missing prepare request")
	}
	workspace := preparedWorkspace(request.FunctionID)
	if err := os.RemoveAll(workspace); err != nil {
		return &firecracker.GuestPrepareResponse{Prepared: false}, err
	}
	if err := nodeexec.PrepareWorkspace(request.ArtifactBundle, workspace); err != nil {
		return &firecracker.GuestPrepareResponse{Prepared: false}, err
	}
	entrypoint := filepath.Join(workspace, filepath.FromSlash(request.Entrypoint))
	if _, err := os.Stat(entrypoint); err != nil {
		return &firecracker.GuestPrepareResponse{Prepared: false}, fmt.Errorf("prepared entrypoint %s: %w", request.Entrypoint, err)
	}
	workerPrepared := false
	if len(request.Env) == 0 {
		workerCtx, cancel := context.WithTimeout(context.Background(), preparedWorkerStartupTimeout)
		defer cancel()
		if err := nodeexec.StartPreparedWorker(workerCtx, nodeexec.PrepareWorkerRequest{
			FunctionID:     request.FunctionID,
			Workspace:      workspace,
			Entrypoint:     request.Entrypoint,
			Env:            request.Env,
			NodeBinary:     guestNodeBinary,
			StartupTimeout: preparedWorkerStartupTimeout,
		}); err != nil {
			return &firecracker.GuestPrepareResponse{
				Prepared:       true,
				WorkerPrepared: false,
				Logs:           err.Error(),
			}, nil
		}
		workerPrepared = true
	}
	return &firecracker.GuestPrepareResponse{Prepared: true, WorkerPrepared: workerPrepared}, nil
}

func execute(request *firecracker.GuestInvocationRequest) (*firecracker.GuestInvocationResponse, error) {
	if request == nil {
		return &firecracker.GuestInvocationResponse{
			ExitCode:   1,
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
		}, fmt.Errorf("missing invocation request")
	}

	var (
		result *nodeexec.Result
		err    error
	)
	workspaceReq := nodeexec.WorkspaceRequest{
		AttemptID:  request.AttemptID,
		JobID:      request.JobID,
		FunctionID: request.FunctionID,
		Workspace:  preparedWorkspace(request.FunctionID),
		Entrypoint: request.Entrypoint,
		Payload:    request.Payload,
		Env:        request.Env,
		Timeout:    time.Duration(request.TimeoutMillis) * time.Millisecond,
		Region:     request.Region,
		HostID:     request.HostID,
		NodeBinary: guestNodeBinary,
	}
	if request.UsePreparedRoot {
		result, err = nodeexec.ExecutePreparedWorker(context.Background(), workspaceReq)
		if err != nil && errors.Is(err, nodeexec.ErrPreparedWorkerUnavailable) {
			result, err = nodeexec.ExecuteWorkspace(context.Background(), workspaceReq)
		}
	} else {
		result, err = nodeexec.ExecuteBundle(context.Background(), nodeexec.Request{
			AttemptID:      request.AttemptID,
			JobID:          request.JobID,
			FunctionID:     request.FunctionID,
			Entrypoint:     request.Entrypoint,
			ArtifactBundle: request.ArtifactBundle,
			Payload:        request.Payload,
			Env:            request.Env,
			Timeout:        time.Duration(request.TimeoutMillis) * time.Millisecond,
			Region:         request.Region,
			HostID:         request.HostID,
			NodeBinary:     guestNodeBinary,
		})
	}
	if result == nil {
		result = &nodeexec.Result{
			ExitCode:   1,
			StartedAt:  time.Now().UTC(),
			FinishedAt: time.Now().UTC(),
		}
	}
	return &firecracker.GuestInvocationResponse{
		ExitCode:   result.ExitCode,
		Logs:       result.Logs,
		Output:     result.Output,
		StartedAt:  result.StartedAt,
		FinishedAt: result.FinishedAt,
	}, err
}

func preparedWorkspace(functionID string) string {
	return filepath.Join(preparedRoot, sanitizeID(functionID))
}

func sanitizeID(input string) string {
	if input == "" {
		return "unknown"
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

func listenVsock(port uint32) (*vsockListener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	if err := unix.Listen(fd, 1); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}

	return &vsockListener{fd: fd}, nil
}

type vsockListener struct {
	fd int
}

func (l *vsockListener) Accept() (io.ReadWriteCloser, error) {
	connFD, _, err := unix.Accept(l.fd)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(connFD), "vsock-conn")
	conn, err := newFileConn(file)
	if err != nil {
		file.Close()
		return nil, err
	}
	return conn, nil
}

func (l *vsockListener) Close() error {
	return unix.Close(l.fd)
}

func newFileConn(file *os.File) (io.ReadWriteCloser, error) {
	return file, nil
}
