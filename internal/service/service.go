// Package service installs whatsupd as a background service so the daemon starts
// at login and restarts after crashes: a LaunchAgent on macOS, a systemd user
// unit on Linux.
package service

import (
	"fmt"
	"runtime"
)

func unsupported() error {
	return fmt.Errorf("the background service is not supported on %s; run `whatsupd serve` under your own supervisor", runtime.GOOS)
}

// Install writes the service definition and (re)starts it. It returns the
// path of the definition. logFile receives output the daemon writes before
// its own log is open (launchd only; systemd keeps it in the journal).
func Install(args []string, logFile string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return launchdInstall(args, logFile)
	case "linux":
		return systemdInstall(args)
	}
	return "", unsupported()
}

// RemoveLegacy stops and removes the service installed under the old name,
// if there is one, and returns the path of the removed definition.
func RemoveLegacy() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return launchdRemoveLegacy()
	case "linux":
		return systemdRemoveLegacy()
	}
	return "", nil
}

// Stop stops the service without removing its definition.
func Stop() {
	switch runtime.GOOS {
	case "darwin":
		launchdStop()
	case "linux":
		systemdStop()
	}
}

// Restart restarts an installed service (e.g. after installing a new binary).
func Restart() error {
	switch runtime.GOOS {
	case "darwin":
		return launchdRestart()
	case "linux":
		return systemdRestart()
	}
	return unsupported()
}

// Uninstall stops the service and removes its definition.
func Uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return launchdUninstall()
	case "linux":
		return systemdUninstall()
	}
	return unsupported()
}

// Status reports whether the service is installed and running.
func Status() string {
	switch runtime.GOOS {
	case "darwin":
		return launchdStatus()
	case "linux":
		return systemdStatus()
	}
	return "not supported on " + runtime.GOOS
}

// Notes returns advice to print after an install, or "".
func Notes() string {
	if runtime.GOOS == "linux" {
		return systemdNotes()
	}
	return ""
}
