//go:build !linux

package main

import "log"

func main() {
	log.Fatal("lecrev-guest-runner is only supported on linux")
}
