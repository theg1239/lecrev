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
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/theg1239/lecrev/internal/firecracker"
	"github.com/theg1239/lecrev/internal/runtime/nodeexec"
)

func main() {
	port, err := guestPort()
	if err != nil {
		log.Fatal(err)
	}
	if err := mountGuestFilesystems(); err != nil {
		log.Printf("guest runner mount setup failed: %v", err)
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
	for _, path := range []string{"/proc", "/sys", "/tmp"} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
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

	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()

	var request firecracker.GuestInvocationRequest
	if err := json.NewDecoder(conn).Decode(&request); err != nil {
		return err
	}

	response, execErr := execute(request)
	if execErr != nil {
		response.Error = execErr.Error()
	}
	if err := json.NewEncoder(conn).Encode(response); err != nil {
		return err
	}

	time.Sleep(250 * time.Millisecond)
	_ = unix.Reboot(unix.LINUX_REBOOT_CMD_POWER_OFF)
	return nil
}

func execute(request firecracker.GuestInvocationRequest) (*firecracker.GuestInvocationResponse, error) {
	result, err := nodeexec.ExecuteBundle(context.Background(), nodeexec.Request{
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
		NodeBinary:     "node",
	})
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
